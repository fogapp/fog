# frozen_string_literal: true

require_relative "test_helper"
require "json"

class ClassOnlyTest < Minitest::Test
  SECRET = "she ate the leftover lasagne"

  class Safe < StandardError
    def flare_safe_message? = true
  end

  class Truthy < StandardError
    def flare_safe_message? = "yes"
  end

  class Raises < StandardError
    def flare_safe_message? = raise("no")
  end

  # What a controller looks like to the builder.
  Request = Struct.new(:url, :request_method, :filtered_parameters)
  class Controller
    def action_name = "create"
    def request = Request.new("https://fit.lan/meals?note=#{SECRET}", "POST", { "note" => SECRET })
  end

  def setup
    Flare.reset!
    @sender = CollectingSender.new
    c = Flare.config
    c.dsn = "http://k@fog:8080/ingest/1"
    c.sender = @sender
    c.app_root = File.expand_path("..", __dir__)
    c.message_policy = :class_only
  end

  def teardown
    Flare.reset!
  end

  def caught(klass = ArgumentError, message = SECRET)
    raise klass, message
  rescue klass => e
    e
  end

  def report(error = caught, **kw)
    Flare.notify(error, **kw)
    Flare.flush(timeout: 2)
    @sender.events.last
  end

  def test_the_default_keeps_messages
    Flare.config.message_policy = :full
    assert_includes report.to_json, SECRET
  end

  def test_the_value_is_empty_and_the_class_stays
    value = report[:exception][:values].last
    assert_equal "", value[:value]
    assert_equal "ArgumentError", value[:type]
    refute_empty value[:stacktrace][:frames]
  end

  def test_every_cause_is_held_to_it
    error = begin
      begin
        raise IOError, SECRET
      rescue IOError
        raise ArgumentError, SECRET
      end
    rescue ArgumentError => e
      e
    end
    assert_equal ["", ""], report(error)[:exception][:values].map { |v| v[:value] }
  end

  def test_a_safe_exception_keeps_its_message
    assert_equal "too many requests", report(caught(Safe, "too many requests"))[:exception][:values].last[:value]
  end

  def test_only_true_counts
    assert_equal "", report(caught(Truthy, SECRET))[:exception][:values].last[:value]
  end

  def test_a_raising_flare_safe_message_counts_as_no
    assert_equal "", report(caught(Raises, SECRET))[:exception][:values].last[:value]
  end

  def test_the_message_of_a_safe_exception_does_not_free_its_cause
    error = begin
      begin
        raise IOError, SECRET
      rescue IOError
        raise Safe, "too many requests"
      end
    rescue Safe => e
      e
    end
    assert_equal ["", "too many requests"], report(error)[:exception][:values].map { |v| v[:value] }
  end

  # Anywhere a caller can write the message, the policy has it first.
  def test_context_cannot_carry_it
    json = report(context: {
      note: SECRET, nested: { deep: [SECRET] }, transaction: SECRET,
    }, fingerprint: [SECRET], source: SECRET).to_json
    refute_includes json, SECRET
  end

  def test_the_request_cannot_carry_it
    event = report(context: { controller: Controller.new })
    refute_includes event.to_json, SECRET
    assert_equal({ method: "POST" }, event[:request])
    assert_equal "ClassOnlyTest::Controller#create", event[:transaction]
  end

  # This file's frames are in-app, and the literal in `raise` is on one.
  def test_source_lines_cannot_carry_it
    error = begin
      raise "literal in the source"
    rescue RuntimeError => e
      e
    end
    json = report(error).to_json
    refute_includes json, "literal in the source"
    refute_includes json, "context_line"
  end

  def test_a_job_keeps_its_name_and_loses_the_rest
    job = Struct.new(:queue_name, :job_id, :arguments).new("default", "j1", [SECRET])
    event = report(context: { job: job, args: job.arguments })
    assert_equal "default", event[:extra][:job][:queue]
    refute_includes event.to_json, SECRET
  end

  # A hook can add what it likes; the same list applies to its answer.
  def test_a_hook_cannot_put_content_back
    Flare.config.before_send = lambda do |event|
      event[:breadcrumbs] = { values: [{ message: SECRET }] }
      event[:tags] = { note: SECRET }
      event[:user] = { email: SECRET }
      event[:message] = SECRET
      event[:extra] = { note: SECRET }
      event[:request] = { method: "POST", url: SECRET, data: { note: SECRET } }
      event[:transaction] = SECRET
      event[:fingerprint] = [SECRET]
      event[:exception][:values].last[:value] = SECRET
      event[:exception][:values].last[:stacktrace][:frames].first[:vars] = { note: SECRET }
      event
    end
    event = report
    refute_includes event.to_json, SECRET
    # The report had no request, so the method the hook wrote is not one the
    # library derived.
    assert_nil event[:request]
    assert_equal "ArgumentError", event[:exception][:values].last[:type]
  end

  # Fields on the list hold only what the library put there: a hook that
  # writes free text into one loses that field, it doesn't send the text.
  def test_a_hook_cannot_write_into_a_listed_field
    Flare.config.before_send = lambda do |event|
      %i[event_id timestamp platform level server_name environment release].each { |k| event[k] = SECRET }
      value = event[:exception][:values].last
      value[:type] = SECRET
      value[:mechanism] = { type: SECRET, handled: SECRET }
      frame = value[:stacktrace][:frames].last
      frame.merge!(function: SECRET, filename: SECRET, abs_path: SECRET, lineno: SECRET, in_app: SECRET)
      value[:stacktrace][:frames] << { function: SECRET, filename: "app/x.rb", lineno: 1 }
      event[:request] = { method: SECRET }
      event[:extra] = { job: { class: SECRET, queue: SECRET, job_id: SECRET } }
      event
    end
    event = report(context: { job: Struct.new(:queue_name, :job_id).new("default", "j1") })
    refute_includes event.to_json, SECRET
    %i[event_id timestamp platform level server_name environment release request extra].each do |k|
      refute event.key?(k), "#{k} should have been dropped"
    end
    value = event[:exception][:values].last
    refute value.key?(:type)
    assert_equal({ type: "generic" }, value[:mechanism])
  end

  # Not even by editing the event's own strings in place.
  def test_a_hook_cannot_edit_the_built_strings_in_place
    Flare.config.before_send = lambda do |event|
      event[:server_name] << SECRET if event[:server_name]
      event[:exception][:values].last[:type] << SECRET
      event[:exception][:values].last[:stacktrace][:frames].each { |f| f[:function] << SECRET if f[:function] }
      event
    end
    refute_includes report.to_json, SECRET
  end

  # A value equal in content but of another class is not the library's.
  def test_a_lookalike_value_is_not_the_built_one
    sub = Class.new(String) { def to_json(*) = %("#{SECRET}") }
    Flare.config.before_send = lambda do |event|
      event[:server_name] = sub.new(event[:server_name])
      event[:exception][:values].last[:stacktrace][:frames].last[:lineno] += 0.0
      event
    end
    event = report
    refute_includes event.to_json, SECRET
    refute event.key?(:server_name)
    refute event[:exception][:values].last[:stacktrace][:frames].last.key?(:lineno)
  end

  def test_a_hook_that_changes_nothing_sends_what_was_built
    built = nil
    Flare.config.before_send = lambda do |event|
      built = Marshal.load(Marshal.dump(event))
      event
    end
    event = report
    assert_equal built.keys, event.keys
    assert_equal built[:exception], event[:exception]
    assert_equal built[:server_name], event[:server_name]
  end

  def test_a_hook_can_drop_and_reorder_what_was_built
    error = begin
      begin
        raise IOError, SECRET
      rescue IOError
        raise ArgumentError, SECRET
      end
    rescue ArgumentError => e
      e
    end
    Flare.config.before_send = lambda do |event|
      event[:exception][:values].reverse!
      event[:exception][:values].each { |v| v[:stacktrace][:frames].pop }
      event.delete(:server_name)
      event
    end
    built_frames = error.backtrace_locations.size
    event = report(error)
    assert_equal %w[ArgumentError IOError], event[:exception][:values].map { |v| v[:type] }
    assert_equal built_frames - 1, event[:exception][:values].first[:stacktrace][:frames].size
    refute event.key?(:server_name)
  end

  def test_a_request_method_the_library_derived_survives_a_hook
    Flare.config.before_send = ->(event) { event }
    assert_equal({ method: "POST" }, report(context: { controller: Controller.new })[:request])
  end

  def test_frames_keep_functions_that_read_as_methods
    functions = report[:exception][:values].last[:stacktrace][:frames].map { |f| f[:function] }
    assert_includes functions, "ClassOnlyTest#caught"
  end

  def test_a_label_that_is_not_a_method_name_is_dropped
    frame = { filename: "app/x.rb", lineno: 3, in_app: true }
    event = { exception: { values: [{ type: "RuntimeError", value: "", stacktrace: { frames: [
      frame.merge(function: SECRET), frame.merge(function: "x" * 300), frame.merge(function: 7),
      frame.merge(function: "block (2 levels) in Foo::Bar#baz?"), frame.merge(function: "<top (required)>"),
      frame.merge(function: "Foo.[]="), frame.merge(function: "<class:Foo>"),
    ] } }] } }
    out = Flare::ClassOnly.restrict(event)[:exception][:values].first[:stacktrace][:frames].map { |f| f[:function] }
    assert_equal [nil, nil, nil, "block (2 levels) in Foo::Bar#baz?", "<top (required)>", "Foo.[]=", "<class:Foo>"], out
  end

  def test_a_hook_can_only_set_a_known_level
    Flare.config.before_send = ->(event) { event.merge(level: :info) }
    assert_equal "info", report[:level]
    Flare.config.before_send = ->(event) { event.merge(level: SECRET) }
    refute report.key?(:level)
  end

  def test_a_hook_can_still_drop_or_retune_an_event
    Flare.config.before_send = ->(event) { event.merge(level: "warning") }
    assert_equal "warning", report[:level]
    Flare.config.before_send = ->(_event) { nil }
    count = @sender.events.size
    Flare.notify(caught)
    Flare.flush(timeout: 2)
    assert_equal count, @sender.events.size
  end

  def test_a_hook_cannot_give_an_unsafe_exception_a_safe_ones_slot
    Flare.config.before_send = lambda do |event|
      event[:exception][:values].first[:value] = "too many requests"
      event
    end
    event = report(caught(Safe, "too many requests"))
    assert_equal "too many requests", event[:exception][:values].last[:value]
  end

  # eval names its frames whatever it is told to: the name is not a path.
  def test_an_eval_filename_does_not_leave
    error = begin
      eval("raise ArgumentError, 'x'", binding, "Dear Bob, #{SECRET}", 1) # rubocop:disable Security/Eval
    rescue ArgumentError => e
      e
    end
    fake_in_app = begin
      eval("raise ArgumentError, 'x'", binding, File.join(Flare.config.app_root, "lasagne_for_bob.rb"), 1) # rubocop:disable Security/Eval
    rescue ArgumentError => e
      e
    end
    [error, fake_in_app].each do |err|
      frames = report(err)[:exception][:values].last[:stacktrace][:frames]
      json = frames.to_json
      refute_includes json, SECRET
      refute_includes json, "lasagne_for_bob"
      assert_equal Flare::ClassOnly::EVAL, frames.last[:abs_path]
      assert_equal Flare::ClassOnly::EVAL, frames.last[:filename]
    end
  end

  # Real frames keep their real paths: this file's, and the gems'.
  def test_real_source_paths_stay
    frames = report[:exception][:values].last[:stacktrace][:frames]
    assert_equal "test/class_only_test.rb", frames.last[:filename]
    assert_equal __FILE__, frames.last[:abs_path]
    assert(frames.any? { |f| f[:abs_path].include?("/gems/minitest") }, "a gem frame lost its path")
  end

  def test_a_path_outside_every_root_or_with_free_text_is_eval
    roots = ["/srv/app"]
    path = ->(p) { Flare::ClassOnly.source_path(p, roots) }
    assert_equal "<internal:kernel>", path.("<internal:kernel>")
    ["/etc/passwd", "/srv/app/Dear Bob.rb", "/srv/app/../etc/x.rb", "/srv/app/a,b.rb", "(eval at /srv/app/x.rb:3)",
     "/srv/application/x.rb", nil, 5, "/srv/app/#{"a" * 2000}.rb"].each do |p|
      assert_equal Flare::ClassOnly::EVAL, path.(p), p.inspect
    end
  end

  # A controller's action name and a job's queue come from the app's code
  # and config; anything that isn't one plain name doesn't leave.
  def test_transaction_and_queue_must_be_names
    controller = Class.new(Controller) { def action_name = "create #{SECRET}" }
    job = Struct.new(:queue_name, :job_id).new("for #{SECRET}", "id with spaces")
    event = report(context: { controller: controller.new })
    refute event.key?(:transaction)
    event = report(context: { job: job })
    refute_includes event.to_json, SECRET
    # An anonymous class has no name either, so nothing of the job is left.
    refute event.key?(:extra)

    ok = Struct.new(:queue_name, :job_id).new("active_storage.analysis", "9f8e-1a2b")
    assert_equal({ queue: "active_storage.analysis", job_id: "9f8e-1a2b" }, report(context: { job: ok })[:extra][:job].slice(:queue, :job_id))
    assert_equal "ClassOnlyTest::Controller#create", report(context: { controller: Controller.new })[:transaction]
  end

  # flare's own failure log, in :full: redacted first, then cut to 500, so a
  # credential spanning the cut doesn't leave its first half.
  def test_a_failure_log_is_redacted_before_it_is_cut
    Flare.config.message_policy = :full
    logged = []
    logger = Object.new
    logger.define_singleton_method(:warn) { |m| logged << m }
    Flare.config.logger = logger
    long = "#{"x" * 440} postgres://app:#{"hunter2" * 100}@db/prod"
    # An exception whose own message raises: notify's rescue logs that error.
    boom = Class.new(StandardError) { define_method(:message) { raise RuntimeError, long } }
    Flare.notify(boom.new)
    assert_equal 1, logged.size
    refute_includes logged.first, "hunter2"
    assert_includes logged.first, "postgres://[FILTERED]"
  end

  def test_a_mistyped_policy_fails_shut
    [:class, "classonly", :none].each do |policy|
      Flare.config.message_policy = policy
      assert Flare.config.class_only?, policy.inspect
    end
    [:full, "full", nil].each do |policy|
      Flare.config.message_policy = policy
      refute Flare.config.class_only?, policy.inspect
    end
  end

  def test_a_failure_inside_notify_does_not_log_the_message
    logged = []
    logger = Object.new
    logger.define_singleton_method(:warn) { |m| logged << m }
    Flare.config.logger = logger
    Flare.config.sender = ->(_e) { raise SECRET }
    Flare.config.before_send = ->(_e) { raise SECRET }
    report
    refute(logged.any? { |l| l.include?(SECRET) })
  end

  def test_the_survivors_are_only_what_the_policy_lists
    event = report(context: { a: 1 })
    assert_empty event.keys - Flare::ClassOnly::TOP_LEVEL
    event[:exception][:values].each do |v|
      assert_empty v.keys - %i[type value mechanism stacktrace]
      v[:stacktrace][:frames].each { |f| assert_empty f.keys - Flare::ClassOnly::FRAME }
    end
  end
end
