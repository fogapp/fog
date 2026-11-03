# frozen_string_literal: true

require_relative "test_helper"

class TransportTest < Minitest::Test
  def config(queue_size: 100)
    c = Flare::Config.new
    c.dsn = "http://k@fog:8080/ingest/1"
    c.queue_size = queue_size
    c
  end

  def test_enqueue_delivers_through_sender
    sender = CollectingSender.new
    t = Flare::Transport.new(config, sender: sender)
    t.enqueue({ event_id: "aa" })
    t.enqueue({ event_id: "bb" })
    t.flush(timeout: 2)
    assert_equal %w[aa bb], sender.events.map { |e| e[:event_id] }
  ensure
    t&.shutdown
  end

  def test_flush_waits_for_in_flight_delivery
    # One slow event: the worker pops it immediately (queue now empty), then
    # spends 0.2s in deliver. flush must wait out that in-flight POST — the
    # at_exit path depends on it, or the final crash report is lost.
    sender = CollectingSender.new(delay: 0.2)
    t = Flare::Transport.new(config, sender: sender)
    t.enqueue({ event_id: "last" })
    t.flush(timeout: 2)
    assert_equal %w[last], sender.events.map { |e| e[:event_id] },
      "flush returned while the last event was still being delivered"
  ensure
    t&.shutdown
  end

  def test_overflow_drops_instead_of_blocking
    # A sender that never finishes the first delivery while we flood the
    # queue: enqueue must return immediately and shed the excess. The gate
    # opens in ensure so shutdown can join instead of killing.
    gate = Queue.new
    sender = ->(_event) { gate.pop; nil }
    t = Flare::Transport.new(config(queue_size: 3), sender: sender)
    done = Queue.new
    Thread.new do
      10.times { |i| t.enqueue({ event_id: i }) }
      done.push(:ok)
    end
    assert_equal :ok, done.pop(timeout: 2), "enqueue must never block the app thread"
    assert t.dropped.positive?, "overflow should be counted as dropped"
  ensure
    gate&.close
    t&.shutdown
  end

  def test_full_reads_without_blocking_and_notify_sheds_before_building
    # A sender blocked on a gate: the queue fills and stays full until the
    # ensure opens the gate (so shutdown can join instead of killing).
    gate = Queue.new
    sender = ->(_event) { gate.pop; nil }
    Flare.reset!
    Flare.config.dsn = "http://k@fog:8080/ingest/1"
    Flare.config.sender = sender
    t = Flare.transport
    refute t.full?, "an empty transport must not read as full"
    (Flare.config.queue_size + 2).times { |i| t.enqueue({ event_id: i }) }
    assert t.full?, "a saturated queue must read as full"
    dropped_before = t.dropped
    err = begin
      raise "storm"
    rescue RuntimeError => e
      e
    end
    assert_nil Flare.notify(err)
    assert_equal dropped_before + 1, t.dropped,
      "an event shed before building must still be counted as dropped"
  ensure
    gate&.close # unblock the sender so shutdown joins instead of killing
    Flare.reset!
  end

  def test_shutdown_finishes_in_flight_delivery_instead_of_killing_it
    # Old behavior: Thread#kill could cut the worker mid-POST. Now shutdown
    # closes the queue and joins: a delivery already started (and the short
    # queue behind it) completes before the worker exits.
    sender = CollectingSender.new(delay: 0.1)
    t = Flare::Transport.new(config, sender: sender)
    3.times { |i| t.enqueue({ event_id: i }) }
    sleep 0.05 # let the worker start delivering the first event
    t.shutdown
    assert_equal [0, 1, 2], sender.events.map { |e| e[:event_id] },
      "shutdown must let started work finish, not kill it mid-delivery"
  end

  def test_transport_survives_shutdown_and_revival
    sender = CollectingSender.new
    t = Flare::Transport.new(config, sender: sender)
    t.enqueue({ event_id: "one" })
    t.flush(timeout: 2)
    t.shutdown
    # Test-only revival (Flare.reset! then reuse): the fresh queue must
    # accept work and a new worker must drain it.
    t.enqueue({ event_id: "two" })
    t.flush(timeout: 2)
    assert_equal %w[one two], sender.events.map { |e| e[:event_id] }
  ensure
    t&.shutdown
  end

  def test_notify_is_inert_without_dsn
    Flare.reset!
    Flare.config.dsn = nil
    begin
      raise "boom"
    rescue RuntimeError => e
      assert_nil Flare.notify(e)
    end
  ensure
    Flare.reset!
  end

  # A stand-in for a Rails that appeared after flare was required (Gemfile
  # lists flare above rails): responds to .error with a recording reporter.
  class FakeErrorReporter
    def subscribers = (@subscribers ||= [])
    def subscribe(s) = subscribers << s
  end

  def test_late_rails_hook_subscribes_once_when_rails_appears_after_require
    # Rails::Railtie was not defined when test_helper required flare, so the
    # railtie never loaded — the exact wrong-Gemfile-order situation. The
    # first configure/notify must repair it by subscribing directly.
    refute defined?(Flare::Railtie), "test premise: railtie must not have loaded"
    Flare.reset!
    reporter = FakeErrorReporter.new
    Object.const_set(:Rails, Module.new)
    Rails.define_singleton_method(:error) { reporter }
    Flare.configure { |c| c.dsn = nil }
    assert_equal 1, reporter.subscribers.size, "first configure must subscribe"
    assert_instance_of Flare::Subscriber, reporter.subscribers.first
    Flare.configure { |c| c.dsn = nil }
    Flare.notify(RuntimeError.new("x"))
    assert_equal 1, reporter.subscribers.size, "the hook must attach exactly once"
  ensure
    Object.send(:remove_const, :Rails)
    Flare.reset!
  end

  def test_subscriber_forwards_to_notify
    Flare.reset!
    Flare.config.dsn = "http://k@fog:8080/ingest/1"
    sender = CollectingSender.new
    Flare.config.sender = sender
    begin
      raise "boom"
    rescue RuntimeError => e
      Flare::Subscriber.new.report(e, handled: true, severity: :warning, context: { a: 1 }, source: "application.active_job")
    end
    Flare.flush(timeout: 2)
    assert_equal 1, sender.events.size
    event = sender.events.first
    assert_equal "warning", event[:level]
    assert_equal 1, event[:extra][:a]
    assert_equal true, event[:exception][:values].last[:mechanism][:handled]
  ensure
    Flare.reset!
  end
end
