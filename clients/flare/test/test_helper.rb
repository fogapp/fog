# frozen_string_literal: true

require "minitest/autorun"
require_relative "../lib/flare"

# A sender that records instead of POSTing, for transport tests. `times`
# records a monotonic timestamp per delivery so tests can assert the worker
# honored a 429 pause (`pause:` is returned from call, exactly like
# HttpSender returning a parsed Retry-After).
class CollectingSender
  attr_reader :events, :times

  def initialize(pause: nil, delay: nil)
    @events = []
    @times = []
    @pause = pause
    @delay = delay
    @mutex = Mutex.new
  end

  def call(event)
    sleep @delay if @delay
    @mutex.synchronize do
      @events << event
      @times << Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end
    @pause
  end
end
