# frozen_string_literal: true

# Flare: error reporting to Fog's built-in tracker (fog:clients/flare/README.md).
#
# The whole client in one sentence: a Rails.error subscriber builds
# Sentry-schema events and a single background thread POSTs them to the
# FLARE_DSN Fog injects — bounded queue, drop on overflow, so reporting can
# never hurt the app doing the reporting. No configuration is required; no
# DSN means the gem is inert (development, test, apps run outside Fog).
#
# Wire-compatible with Sentry on purpose: point the official sentry-ruby SDK
# at the same DSN and the server can't tell the difference.

require_relative "flare/version"
require_relative "flare/class_only"
require_relative "flare/config"
require_relative "flare/event_builder"
require_relative "flare/transport"
require_relative "flare/subscriber"
require_relative "flare/railtie" if defined?(Rails::Railtie)

module Flare
  # Guards lazy singleton construction: concurrent first errors must not
  # each build a Transport (orphaning a queue and worker thread).
  LOCK = Mutex.new
  private_constant :LOCK

  class << self
    def config
      @config || LOCK.synchronize { @config ||= Config.new }
    end

    def configure
      yield config
      attach_rails_late
    end

    def enabled?
      config.enabled?
    end

    # Manual reporting for rescued errors the Rails reporter never sees
    # (or for use outside Rails entirely). Never raises.
    def notify(exception, handled: true, severity: :error, context: {}, source: nil, fingerprint: nil)
      attach_rails_late
      return nil unless enabled? && exception.is_a?(Exception)
      t = transport
      # Error storm: with the queue already full the event would be built
      # (frame mapping, file reads, scrubbing — all on this thread) only to
      # be dropped at enqueue. Shed first. full? is a relaxed, lock-free
      # read; a stale answer costs one borderline event, never a deadlock.
      return t.count_drop if t.full?
      event = EventBuilder.new(config).build(
        exception, handled: handled, severity: severity, context: context,
        source: source, fingerprint: fingerprint
      )
      event = through_before_send(event)
      t.enqueue(event) if event
      nil
    rescue StandardError => e
      detail = config.class_only? ? e.class.to_s : "#{e.class}: #{config.redact(e.message.to_s)[0, 500]}"
      config.logger&.warn("flare: notify failed: #{detail}")
      nil
    end

    # Attach key/values to every report in the current execution (delegates
    # to Rails' reporter context when available).
    def context(**kv)
      if defined?(Rails) && Rails.respond_to?(:error)
        Rails.error.set_context(**kv)
      end
      nil
    end

    # Drain the queue, waiting at most `timeout` seconds. Called at exit so
    # a crash-then-terminate still reports.
    def flush(timeout: 2)
      @transport&.flush(timeout: timeout)
    end

    def transport
      # Resolve config before taking LOCK — config may take it too, and the
      # Mutex is not reentrant.
      @transport || begin
        cfg = config
        LOCK.synchronize { @transport ||= Transport.new(cfg) }
      end
    end

    # Test hook: forget all state, including the worker thread.
    def reset!
      LOCK.synchronize do
        @transport&.shutdown
        @transport = nil
        @config = nil
        @rails_attached = nil
      end
    end

    private

    # The app's last word on an event: it gets the scrubbed event, and what it
    # answers is what goes out, nil for nothing. A hook that fails, or answers
    # something that isn't an event, costs that event and nothing else. The
    # event goes unsent rather than unhooked, because the hook may be what
    # takes the private part out. ScriptError and the like are caught as well
    # as StandardError: a hook that fails to load must not take a request with
    # it. Its message stays out of the log, since it may quote the event.
    def through_before_send(event)
      hook = config.before_send
      return event unless hook
      allowed = ClassOnly.allowed(event) if config.class_only?
      answer = begin
        hook.call(event)
      rescue StandardError, ScriptError, SystemStackError, NoMemoryError => e
        warn_quietly("flare: before_send raised #{e.class}; event dropped")
        return nil
      end
      return nil if answer.nil?
      unless answer.is_a?(Hash)
        warn_quietly("flare: before_send answered a #{answer.class}, not an event; event dropped")
        return nil
      end
      # The hook can't widen the policy: what it added goes the same way as
      # what it was given.
      allowed ? ClassOnly.restrict(answer, allowed: allowed) : answer
    end

    def warn_quietly(message)
      config.logger&.warn(message)
    rescue StandardError
      nil
    end

    # Best-effort repair for Gemfile ordering: with `gem "flare"` listed
    # above `gem "rails"`, Rails::Railtie is not defined when flare is
    # required, the railtie never loads, and the gem would be silently inert
    # in a Rails app. So the first configure/notify checks again: if Rails
    # has appeared since require time, register the same subscriber the
    # railtie would have (inert without a DSN, exactly like the railtie's
    # unconditional subscribe). No-op when the railtie did load, outside
    # Rails, and on every call after the first. Boot-time errors are still
    # only covered by the railtie — hence the README's ordering note.
    def attach_rails_late
      return if @rails_attached || defined?(Flare::Railtie)
      return unless defined?(Rails) && Rails.respond_to?(:error)
      # Resolve config before taking LOCK — config may take it, and the
      # Mutex is not reentrant.
      cfg = config
      LOCK.synchronize do
        return if @rails_attached
        @rails_attached = true
        Rails.error.subscribe(Subscriber.new)
        if cfg.enabled?
          cfg.logger ||= Rails.logger if Rails.respond_to?(:logger)
          if Rails.respond_to?(:application) && Rails.application
            cfg.rails_filters = Rails.application.config.filter_parameters || []
          end
        end
      end
      nil
    end
  end
end

at_exit { Flare.flush(timeout: 2) if Flare.enabled? }
