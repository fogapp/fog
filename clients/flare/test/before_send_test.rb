# frozen_string_literal: true

require_relative "test_helper"

class BeforeSendTest < Minitest::Test
  def setup
    Flare.reset!
    @sender = CollectingSender.new
    Flare.config.dsn = "http://k@fog:8080/ingest/1"
    Flare.config.sender = @sender
  end

  def teardown
    Flare.reset!
  end

  def report(error = boom, **kw)
    assert_nil Flare.notify(error, **kw)
    Flare.flush(timeout: 2)
    @sender.events
  end

  def boom
    raise ArgumentError, "the message"
  rescue ArgumentError => e
    e
  end

  def test_no_hook_sends_the_event
    assert_equal 1, report.size
  end

  def test_a_hook_can_change_the_event
    Flare.config.before_send = ->(event) { event.merge(level: "warning") }
    assert_equal "warning", report.first[:level]
  end

  def test_a_hook_can_change_it_in_place
    Flare.config.before_send = lambda do |event|
      event[:extra].delete(:meal)
      event
    end
    assert_equal({ kept: 1 }, report(context: { meal: "soup", kept: 1 }).first[:extra])
  end

  def test_nil_drops_the_event
    Flare.config.before_send = ->(_event) { nil }
    assert_empty report
    assert_equal 0, Flare.transport.dropped
  end

  # The hook sees what scrubbing left, not what the app passed in.
  def test_the_hook_runs_after_scrub
    seen = nil
    Flare.config.before_send = lambda do |event|
      seen = event
      event
    end
    report(context: { password: "hunter2" })
    assert_equal "[FILTERED]", seen[:extra][:password]
  end

  def test_a_hook_that_raises_never_reaches_the_app_and_loses_the_event
    [RuntimeError.new("no"), NotImplementedError.new("no"), LoadError.new("no"), SystemStackError.new("no")].each do |error|
      Flare.config.before_send = ->(_event) { raise error }
      assert_empty report
    end
  end

  def test_a_hook_failure_logs_the_class_and_not_the_message
    logged = []
    logger = Object.new
    logger.define_singleton_method(:warn) { |m| logged << m }
    Flare.config.logger = logger
    Flare.config.before_send = ->(_event) { raise "quoted content" }
    report
    assert_equal 1, logged.size
    assert_includes logged.first, "RuntimeError"
    refute_includes logged.first, "quoted content"
  end

  def test_a_logger_that_raises_changes_nothing
    logger = Object.new
    logger.define_singleton_method(:warn) { |_m| raise "disk full" }
    Flare.config.logger = logger
    Flare.config.before_send = ->(_event) { raise "no" }
    assert_empty report
  end

  def test_an_answer_that_is_not_an_event_loses_the_event
    [true, "event", 1, []].each do |answer|
      Flare.config.before_send = ->(_event) { answer }
      assert_empty report
    end
  end

  def test_any_callable_will_do
    hook = Object.new
    hook.define_singleton_method(:call) { |event| event.merge(release: "hooked") }
    Flare.config.before_send = hook
    assert_equal "hooked", report.first[:release]
  end
end
