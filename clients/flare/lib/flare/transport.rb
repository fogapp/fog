# frozen_string_literal: true

require "json"
require "net/http"
require "time"
require "uri"

module Flare
  # One background thread draining a bounded queue. The contract that makes
  # this safe to run inside every app: enqueue never blocks (overflow drops),
  # delivery failures drop (never retry into a dead server), and a 429's
  # Retry-After pauses the worker so fog can shed load with one header.
  class Transport
    attr_reader :dropped

    def initialize(config, sender: nil)
      @config = config
      @sender = sender || config.sender || HttpSender.new(config)
      @queue = SizedQueue.new(config.queue_size)
      @dropped = 0
      @mutex = Mutex.new
      @idle = ConditionVariable.new
      @pending = 0 # events enqueued but not yet fully delivered (or dropped)
      @thread = nil
      @pid = nil
    end

    # Cheap backpressure probe for Flare.notify: when the queue is already
    # full, building the event (frame mapping, file reads, scrubbing) would
    # be pure waste — it gets dropped on push anyway. Deliberately relaxed:
    # no lock, so a racing answer can be one event stale, which only means
    # one borderline event gets built or shed — never a deadlock, never a
    # block. A fork (stale @pid) reads as not-full because the next enqueue
    # replaces the inherited queue.
    def full?
      @pid == Process.pid && @queue.size >= @queue.max
    end

    # Lets notify count an event it shed before building it, so `dropped`
    # stays the one number for "events flare gave up on". Every increment of
    # @dropped takes @mutex: notify threads and the worker both count drops,
    # and a lost update would quietly understate how much flare shed.
    def count_drop
      @mutex.synchronize { @dropped += 1 }
      nil
    end

    def enqueue(event)
      ensure_worker
      # @pending covers the event from before it enters the queue until
      # deliver returns, so flush can't miss work the queue no longer shows.
      @mutex.synchronize { @pending += 1 }
      begin
        @queue.push(event, true)
      rescue ThreadError # queue full — shed, don't block the app
        @mutex.synchronize do
          @pending -= 1
          @idle.broadcast if @pending.zero?
          @dropped += 1
        end
        nil
      end
    end

    # Waits for queued AND in-flight events: the worker keeps an event
    # counted in @pending until deliver returns, so flush returning means the
    # final event (usually the crash the at_exit hook exists for) was handed
    # to the sender — not merely popped off the queue.
    def flush(timeout: 2)
      deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
      @mutex.synchronize do
        while @pending.positive?
          remaining = deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC)
          break if remaining <= 0
          @idle.wait(@mutex, remaining)
        end
      end
      nil
    end

    # Reachable via Flare.reset!: stop the worker without cutting an
    # in-flight POST mid-body. Closing the queue wakes a blocked pop with
    # nil after the remaining events drain; the worker exits on its own.
    # Kill is the last resort for a worker wedged in a slow deliver or a
    # long 429 pause.
    def shutdown
      thread = @thread
      @thread = nil
      if thread&.alive?
        @queue.close
        thread.kill unless thread.join(2)
      end
      # A closed queue can't accept pushes again; give a (test-only)
      # revived transport a fresh one, with nothing pending on it.
      @queue = SizedQueue.new(@config.queue_size)
      @mutex.synchronize do
        @pending = 0
        @idle.broadcast
      end
    end

    private

    # Puma and Solid Queue fork; threads don't survive a fork. Rather than
    # asking every app to add an after-fork hook, detect the new pid on the
    # next enqueue and start fresh (dropping any events inherited from the
    # parent — the parent's worker owns those).
    def ensure_worker
      return if @thread&.alive? && @pid == Process.pid
      @mutex.synchronize do
        return if @thread&.alive? && @pid == Process.pid
        if @pid && @pid != Process.pid
          @queue = SizedQueue.new(@config.queue_size)
          @pending = 0 # inherited events belong to the parent's worker
        end
        @pid = Process.pid
        @thread = Thread.new { work }
        @thread.name = "flare"
      end
    end

    # Exits when shutdown closes the queue: pop hands over the remaining
    # events, then returns nil.
    def work
      while (event = @queue.pop)
        pause = deliver(event)
        @mutex.synchronize do
          @pending -= 1
          @idle.broadcast if @pending.zero?
        end
        sleep(pause) if pause&.positive?
      end
    end

    # Returns seconds to pause (server said 429), or nil. The pause is
    # already bounded — the sender owns the Retry-After clamp.
    def deliver(event)
      @sender.call(event)
    rescue StandardError
      @mutex.synchronize { @dropped += 1 }
      nil
    end
  end

  # POSTs one event per envelope. Sized for a LAN hop to fog: short
  # timeouts, no retries, no connection pooling worth the code.
  class HttpSender
    # The one Retry-After bound: whatever the server asks, the worker
    # pauses at most this long (and at least 1s, so a junk header still
    # backs off). The Transport sleeps exactly what the sender returns.
    RETRY_AFTER_RANGE = (1..300)

    def initialize(config)
      @config = config
    end

    def call(event)
      dsn = @config.parsed_dsn or return nil
      payload = JSON.generate(event)
      envelope = +""
      envelope << JSON.generate(event_id: event[:event_id], sent_at: Time.now.utc.iso8601) << "\n"
      envelope << JSON.generate(type: "event", length: payload.bytesize) << "\n"
      envelope << payload << "\n"

      uri = URI.parse(dsn.endpoint)
      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = uri.scheme == "https"
      http.open_timeout = @config.open_timeout
      http.read_timeout = @config.read_timeout
      req = Net::HTTP::Post.new(uri.request_uri)
      req["Content-Type"] = "application/x-sentry-envelope"
      req["X-Sentry-Auth"] =
        "Sentry sentry_version=7, sentry_client=flare-ruby/#{VERSION}, sentry_key=#{dsn.key}"
      req.body = envelope
      resp = http.request(req)
      resp.code == "429" ? (resp["Retry-After"] || "60").to_i.clamp(RETRY_AFTER_RANGE) : nil
    end
  end
end
