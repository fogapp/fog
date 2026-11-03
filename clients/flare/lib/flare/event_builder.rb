# frozen_string_literal: true

require "json"
require "securerandom"
require "socket"
require "time"
require "uri"

module Flare
  # Builds a Sentry-schema event hash from an exception plus whatever
  # execution context the Rails reporter handed over. Runs on the caller's
  # thread, so everything here is bounded: capped cause chains, capped
  # messages, a capped source-file cache.
  class EventBuilder
    MAX_CAUSES = 5
    MAX_MESSAGE = 2000

    # Backtrace bounds, Sentry-style middle trim: a deep-recursion error
    # carries ~10k frames, and mapping them all builds a multi-MB event on
    # the request thread (×MAX_CAUSES for a cause chain). Keep the oldest
    # frames (program entry) and the newest (crash site); the elided middle
    # is the recursion repeating itself.
    MAX_FRAMES_HEAD = 50  # oldest frames kept, entry side
    MAX_FRAMES_TAIL = 150 # newest frames kept, crash side
    # Source context (pre/post lines) is the expensive per-frame payload;
    # only this many in-app frames per event get it, newest first.
    MAX_CONTEXT_FRAMES = 50

    # Shared source-line cache: in-app frames get a few lines of context, and
    # a hot error would otherwise re-read the same files per occurrence.
    SOURCE_CACHE = {} # path -> Array[String], or nil for unreadable/oversized files
    SOURCE_CACHE_MAX = 100
    SOURCE_MAX_FILE_BYTES = 500_000 # don't hold huge generated files in memory
    SOURCE_MUTEX = Mutex.new

    def initialize(config)
      @config = config
    end

    def build(exception, handled:, severity:, context:, source:, fingerprint:)
      context ||= {}
      @context_budget = MAX_CONTEXT_FRAMES # per event, shared across the cause chain
      event = {
        event_id: SecureRandom.uuid.delete("-"),
        timestamp: Time.now.utc.iso8601,
        platform: "ruby",
        level: level_for(severity),
        server_name: hostname,
        environment: @config.environment,
        release: @config.release,
        transaction: transaction_from(context),
        exception: { values: chain(exception, handled: handled, source: source) },
        request: request_from(context),
        extra: extra_from(context),
      }
      event[:fingerprint] = Array(fingerprint) if fingerprint
      event = event.reject { |_, v| v.nil? || v == "" }
      @config.class_only? ? ClassOnly.restrict(event, roots: ClassOnly.path_roots(@config.app_root)) : event
    end

    private

    def level_for(severity)
      case severity
      when :warning, :warn then "warning"
      when :info then "info"
      else "error"
      end
    end

    def hostname
      Socket.gethostname
    rescue StandardError
      nil
    end

    # Cause chain, Sentry's order: root cause first, the raised exception
    # last. `handled` describes the reported exception; its causes were by
    # definition wrapped, so they carry no mechanism of their own.
    def chain(exception, handled:, source:)
      list = []
      e = exception
      while e.is_a?(Exception) && list.size < MAX_CAUSES && !list.any? { |x| x.equal?(e) }
        list << e
        e = e.cause
      end
      list.reverse.map do |ex|
        value = {
          type: ex.class.name,
          value: message_for(ex),
        }
        if ex.equal?(exception)
          value[:mechanism] = { type: source.to_s.empty? ? "generic" : source.to_s, handled: handled }
        end
        frames = frames_for(ex)
        value[:stacktrace] = { frames: frames } unless frames.empty?
        value
      end
    end

    # A message is code's own words around whatever it interpolated: a
    # connection string, a token. The whole message is redacted
    # (Config#redact), then cut: cut first, a credential running past the
    # cut would lose what marks it as one and leave in part. Each pattern's
    # time limit bounds the cost of a huge message (one that hits it is
    # dropped whole).
    def message_for(exception)
      raw = @config.class_only? ? ClassOnly.message_for(exception) : exception.message.to_s
      @config.redact(raw)[0, MAX_MESSAGE]
    end

    # Ruby backtraces list the crash site first; Sentry frames go the other
    # way (oldest call first, crash last). Oversized backtraces lose their
    # middle before any mapping happens: in Ruby order the first
    # MAX_FRAMES_TAIL locations are the crash side, the last MAX_FRAMES_HEAD
    # the program entry. Mapping runs newest-first so the source-context
    # budget is spent on the frames nearest the crash, then flips to Sentry
    # order.
    def frames_for(ex)
      locs = ex.backtrace_locations || []
      if locs.size > MAX_FRAMES_HEAD + MAX_FRAMES_TAIL
        locs = locs[0, MAX_FRAMES_TAIL] + locs[-MAX_FRAMES_HEAD, MAX_FRAMES_HEAD]
      end
      locs.map { |loc| frame_for(loc) }.reverse
    end

    def frame_for(loc)
      abs = loc.absolute_path || loc.path || "?"
      root = @config.app_root
      in_app = abs.start_with?(root + "/") && !abs.include?("/vendor/") && !abs.include?("/.bundle/")
      frame = {
        filename: in_app ? abs.delete_prefix(root + "/") : abs,
        abs_path: abs,
        function: loc.label,
        lineno: loc.lineno,
        in_app: in_app,
      }
      # Source context only for in-app frames: it's our code, and it's what
      # makes the fog UI show the actual line instead of a path. Budgeted
      # per event — MAX_CONTEXT_FRAMES frames get it, nearest the crash.
      if in_app && !@config.class_only? && @context_budget.positive? && (ctx = source_context(abs, loc.lineno))
        @context_budget -= 1
        frame.merge!(ctx)
      end
      frame
    end

    def source_context(path, lineno)
      lines = SOURCE_MUTEX.synchronize do
        if SOURCE_CACHE.key?(path)
          SOURCE_CACHE[path]
        else
          # At capacity, evict the oldest entry (FIFO — Ruby hashes preserve
          # insertion order) to make room for the new path.
          SOURCE_CACHE.shift if SOURCE_CACHE.size >= SOURCE_CACHE_MAX
          SOURCE_CACHE[path] = read_source(path)
        end
      end
      return nil unless lines
      i = lineno - 1
      return nil if i.negative? || i >= lines.size
      n = @config.context_lines
      {
        pre_context: lines[[i - n, 0].max...i] || [],
        context_line: lines[i],
        post_context: lines[(i + 1)..[i + n, lines.size - 1].min] || [],
      }
    rescue StandardError
      nil
    end

    # nil (cached) for files that are missing or too large to hold onto.
    # Raises from the stat/read land in source_context's rescue.
    # Lines are redacted (Config#redact) as they're read, once per file: a
    # source line is sent beside the frame, and a credential written into
    # the code, or the literal a raise interpolates, leaves with it.
    def read_source(path)
      return nil unless File.exist?(path) && File.size(path) <= SOURCE_MAX_FILE_BYTES
      File.readlines(path, chomp: true).map { |line| @config.redact(line) }
    end

    # The Rails reporter auto-attaches :controller on request errors and
    # :job on ActiveJob errors — the two names that matter for grouping.
    def transaction_from(context)
      if (c = context[:controller]) && c.respond_to?(:action_name)
        "#{c.class.name}##{c.action_name}"
      elsif (j = context[:job])
        j.class.name
      elsif context[:transaction] && !@config.class_only?
        context[:transaction].to_s
      end
    end

    def request_from(context)
      c = context[:controller]
      return nil unless c.respond_to?(:request) && c.request
      r = c.request
      # filtered_parameters applies the app's own filter_parameters config;
      # our scrub is a second pass with the built-in patterns.
      params = r.respond_to?(:filtered_parameters) ? r.filtered_parameters : {}
      {
        url: url_of(r),
        method: r.respond_to?(:request_method) ? r.request_method : nil,
        data: @config.scrub(jsonable(params)),
      }.compact
    rescue StandardError
      nil
    end

    # The request's URL, its query filtered like its params. Rails' own
    # filtered_path goes first where there is one (it runs the app's
    # filter_parameters through ParameterFilter, procs included), then
    # filtered_url adds the built-in patterns.
    def url_of(r)
      url = if r.respond_to?(:filtered_path) && r.respond_to?(:base_url)
              "#{r.base_url}#{r.filtered_path}"
            elsif r.respond_to?(:url)
              r.url
            end
      url && filtered_url(url)
    end

    # The URL's query string holds the same parameters filtered_parameters
    # filters (?token=…), so it gets the same scrub, by Rails' rules: a key
    # is read as Rack nests it (credit_card[number] is credit_card, then
    # number), and is filtered when any level of it is (filtered_key?). A
    # filtered key keeps its name and loses its value. A query that won't
    # decode goes whole, and the URL's userinfo goes too.
    def filtered_url(url)
      base, query = url.to_s.split("?", 2)
      base = @config.redact(base)
      return base if query.nil?
      pairs = query.split("&", -1).map do |pair|
        key, value = pair.split("=", 2)
        value && nested_filtered?(URI.decode_www_form_component(key)) ? "#{key}=#{Config::FILTERED}" : pair
      end
      "#{base}?#{pairs.join("&")}"
    rescue ArgumentError, Encoding::CompatibilityError
      "#{base}?#{Config::FILTERED}"
    end

    # Rack's nesting: "a[b][]" is a, then b (an array adds no level).
    def nested_filtered?(name)
      head, rest = name.split("[", 2)
      segments = [head]
      segments.concat(rest.to_s.scan(/([^\[\]]*)\]/).flatten.reject(&:empty?)) if rest
      segments.each_index.any? { |i| @config.filtered_key?(segments[i], segments[0...i]) }
    end

    # Everything else in the context hash rides along under extra, scrubbed
    # and coerced to JSON-safe values. Job details get stable names.
    def extra_from(context)
      extra = {}
      if (j = context[:job])
        extra[:job] = { class: j.class.name }
        extra[:job][:queue] = j.queue_name if j.respond_to?(:queue_name)
        extra[:job][:job_id] = j.job_id if j.respond_to?(:job_id)
      end
      context.each do |k, v|
        next if k == :controller || k == :job
        extra[k] = jsonable(v)
      end
      return nil if extra.empty?
      @config.scrub(extra)
    end

    # Context values can be arbitrary objects; the payload must be JSON, and
    # the scrub sees keys and string values, so nothing may reach it as an
    # opaque string. A short list of types turns into a Hash (as_hash:
    # ActionController::Parameters by to_unsafe_h, since its to_h refuses
    # unpermitted keys; Struct, Data, OpenStruct by to_h), and the scrub then
    # filters it like any other. Every other object, ENV among them, is its
    # class name and nothing more: its to_s, inspect or to_h is whatever it
    # holds, password included.
    def jsonable(obj, depth = 0)
      return "…" if depth > 6
      case obj
      when Hash then obj.to_h { |k, v| [json_key(k), jsonable(v, depth + 1)] }
      when Array then obj.first(50).map { |v| jsonable(v, depth + 1) }
      when String, Numeric, TrueClass, FalseClass, NilClass then obj
      when Symbol then obj.to_s
      else
        h = as_hash(obj)
        h ? jsonable(h, depth) : opaque(obj)
      end
    rescue StandardError
      nil
    end

    # Only these become hashes. Anything else with a to_h stays opaque:
    # ENV has one, and expanding it would send DATABASE_URL and FLARE_DSN
    # filtered only by their names.
    def as_hash(obj)
      h = if defined?(::ActionController::Parameters) && obj.is_a?(::ActionController::Parameters)
            obj.to_unsafe_h
          elsif obj.is_a?(Struct) || (defined?(::Data) && obj.is_a?(::Data)) ||
                (defined?(::OpenStruct) && obj.is_a?(::OpenStruct))
            obj.to_h
          end
      h.is_a?(Hash) ? h : nil
    rescue StandardError
      nil
    end

    def json_key(key)
      case key
      when String, Symbol, Numeric, TrueClass, FalseClass, NilClass then key.to_s
      else opaque(key)
      end
    end

    # "#<Class>", from the class itself: an object can't choose what it says.
    def opaque(obj)
      klass = Kernel.instance_method(:class).bind_call(obj)
      "#<#{Module.instance_method(:name).bind_call(klass) || "anonymous"}>"
    rescue StandardError
      "#<object>"
    end
  end
end
