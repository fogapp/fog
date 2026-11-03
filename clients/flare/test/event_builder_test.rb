# frozen_string_literal: true

require_relative "test_helper"
require "json"
require "tmpdir"

class EventBuilderTest < Minitest::Test
  def config
    c = Flare::Config.new
    c.dsn = "http://k@fog:8080/ingest/1"
    # This repo is the "app" so the test file's frames count as in-app.
    c.app_root = File.expand_path("..", __dir__)
    c
  end

  def build(exception, **kw)
    defaults = { handled: false, severity: :error, context: {}, source: "application.action_dispatch", fingerprint: nil }
    Flare::EventBuilder.new(config).build(exception, **defaults.merge(kw))
  end

  def raise_nested
    begin
      raise ArgumentError, "root cause"
    rescue ArgumentError
      raise RuntimeError, "wrapper"
    end
  rescue RuntimeError => e
    e
  end

  def test_exception_chain_is_root_cause_first
    event = build(raise_nested)
    values = event[:exception][:values]
    assert_equal %w[ArgumentError RuntimeError], values.map { |v| v[:type] }
    assert_equal "root cause", values[0][:value]
    # Mechanism (handled/source) belongs to the exception that was reported.
    assert_nil values[0][:mechanism]
    assert_equal false, values[1][:mechanism][:handled]
    assert_equal "application.action_dispatch", values[1][:mechanism][:type]
  end

  def test_frames_are_oldest_first_with_in_app_and_context
    event = build(raise_nested)
    frames = event[:exception][:values].last[:stacktrace][:frames]
    crash = frames.last
    # The crash site is this test file: in-app, repo-relative, with source.
    assert crash[:in_app], "crash frame should be in_app: #{crash.inspect}"
    assert_equal "test/event_builder_test.rb", crash[:filename]
    assert_includes crash[:context_line], "raise RuntimeError"
    assert_equal 3, crash[:pre_context].size
  end

  def test_event_is_json_serializable_and_carries_identity
    event = build(raise_nested)
    round = JSON.parse(JSON.generate(event))
    assert_equal "ruby", round["platform"]
    assert_equal "error", round["level"]
    assert_match(/\A[0-9a-f]{32}\z/, round["event_id"])
  end

  def test_transaction_and_job_context
    job = Struct.new(:queue_name, :job_id) do
      def self.name = "SyncFeedJob"
    end.new("default", "j-1")
    event = build(raise_nested, context: { job: job, feed_id: 42 })
    assert_equal "SyncFeedJob", event[:transaction]
    assert_equal "SyncFeedJob", event[:extra][:job][:class]
    assert_equal 42, event[:extra][:feed_id]
  end

  def test_extra_is_scrubbed_and_coerced
    event = build(raise_nested, context: { api_token: "sekrit", thing: Object.new })
    assert_equal Flare::Config::FILTERED, event[:extra][:api_token]
    assert_kind_of String, event[:extra][:thing]
  end

  def test_fingerprint_passthrough
    event = build(raise_nested, fingerprint: ["{{ default }}", "tenant-1"])
    assert_equal ["{{ default }}", "tenant-1"], event[:fingerprint]
  end

  def test_severity_mapping
    assert_equal "warning", build(raise_nested, severity: :warning)[:level]
    assert_equal "error", build(raise_nested, severity: :mystery)[:level]
  end

  # A real exception with a synthetically deep backtrace.
  def deep_raise(n)
    n.zero? ? raise(RuntimeError, "deep") : deep_raise(n - 1)
  end

  def capture_deep(n)
    deep_raise(n)
  rescue RuntimeError => e
    e
  end

  def test_deep_backtraces_are_trimmed_in_the_middle
    head = Flare::EventBuilder::MAX_FRAMES_HEAD
    tail = Flare::EventBuilder::MAX_FRAMES_TAIL
    e = capture_deep(head + tail + 500)
    assert_operator e.backtrace_locations.size, :>, head + tail, "test needs an oversized backtrace"
    event = build(e)
    frames = event[:exception][:values].last[:stacktrace][:frames]
    assert_equal head + tail, frames.size
    # Sentry order survives the trim: the crash site is still the last
    # frame, the outermost caller still the first.
    assert_includes frames.last[:context_line].to_s, "raise"
    refute_equal "deep_raise", frames.first[:function],
      "the head of the trimmed trace should be the program entry, not the recursion"
  end

  def test_source_context_is_budgeted_and_spent_nearest_the_crash
    budget = Flare::EventBuilder::MAX_CONTEXT_FRAMES
    e = capture_deep(Flare::EventBuilder::MAX_FRAMES_HEAD + Flare::EventBuilder::MAX_FRAMES_TAIL + 500)
    event = build(e)
    frames = event[:exception][:values].last[:stacktrace][:frames]
    with_context = frames.select { |f| f.key?(:context_line) }
    assert_operator with_context.size, :<=, budget,
      "source context must stop at the budget, not decorate every in-app frame"
    assert_operator with_context.size, :>, 0
    # The budget goes to the newest frames: the crash site keeps its context.
    assert frames.last.key?(:context_line), "the crash frame must keep its source context"
  end

  def test_source_cache_stays_capped
    Flare::EventBuilder::SOURCE_CACHE.clear
    builder = Flare::EventBuilder.new(config)
    max = Flare::EventBuilder::SOURCE_CACHE_MAX
    Dir.mktmpdir do |dir|
      (max + 1).times do |i|
        path = File.join(dir, "file_#{i}.rb")
        File.write(path, "line one\nline two\n")
        ctx = builder.send(:source_context, path, 1)
        assert_equal "line one", ctx[:context_line]
      end
      assert_operator Flare::EventBuilder::SOURCE_CACHE.size, :<=, max,
        "cache must evict at capacity, not grow without bound"
      # FIFO: the oldest path was evicted, the newest survives.
      refute Flare::EventBuilder::SOURCE_CACHE.key?(File.join(dir, "file_0.rb"))
      assert Flare::EventBuilder::SOURCE_CACHE.key?(File.join(dir, "file_#{max}.rb"))
    end
  ensure
    Flare::EventBuilder::SOURCE_CACHE.clear
  end

  def test_source_cache_skips_oversized_files
    Flare::EventBuilder::SOURCE_CACHE.clear
    builder = Flare::EventBuilder.new(config)
    Dir.mktmpdir do |dir|
      path = File.join(dir, "huge.rb")
      File.write(path, "x" * (Flare::EventBuilder::SOURCE_MAX_FILE_BYTES + 1))
      assert_nil builder.send(:source_context, path, 1)
      assert_nil Flare::EventBuilder::SOURCE_CACHE[path], "oversized file's lines must not be held in memory"
    end
  ensure
    Flare::EventBuilder::SOURCE_CACHE.clear
  end

  Req = Struct.new(:url, :request_method, :filtered_parameters)
  Ctrl = Struct.new(:request) do
    def action_name = "show"
  end

  def url_for(url)
    build(RuntimeError.new("x"), context: { controller: Ctrl.new(Req.new(url, "GET", {})) })[:request][:url]
  end

  # filtered_parameters covers the params; the URL carries them again.
  def test_the_url_query_gets_the_same_scrub_as_params
    assert_equal "https://a.lan/p?token=[FILTERED]&page=2&api_key=[FILTERED]&flag",
                 url_for("https://a.lan/p?token=s3cr3t&page=2&api_key=k&flag")
    assert_equal "https://a.lan/p?user%5Bpassword%5D=[FILTERED]", url_for("https://a.lan/p?user%5Bpassword%5D=hunter2")
    assert_equal "https://a.lan/p", url_for("https://a.lan/p")
  end

  def test_the_url_query_honours_the_apps_filters
    c = config
    c.rails_filters = [:meal]
    event = Flare::EventBuilder.new(c).build(RuntimeError.new("x"), handled: true, severity: :error, source: nil,
      fingerprint: nil, context: { controller: Ctrl.new(Req.new("https://a.lan/?meal=soup&x=1", "GET", {})) })
    assert_equal "https://a.lan/?meal=[FILTERED]&x=1", event[:request][:url]
  end

  def test_a_query_that_will_not_decode_goes_whole
    assert_equal "https://a.lan/p?[FILTERED]", url_for("https://a.lan/p?tok%en=1")
  end

  # ActionController::Parameters as the reviewer met it, where Rails isn't
  # loaded: to_h refuses unpermitted keys, to_unsafe_h answers, and
  # to_s/inspect print it all. (RealRailsTest below uses the real one.)
  unless defined?(::ActionController::Parameters)
    module ::ActionController
      class Parameters
        def initialize(h) = @h = h
        def to_unsafe_h = @h
        def to_h = raise("unpermitted parameters")
        def to_s = @h.to_s
        def inspect = @h.inspect
      end
    end
  end
  FakeParams = ::ActionController::Parameters

  # Not on the list, though it has a to_h: like ENV, it stays opaque.
  class Vault
    def to_h = { "token" => "s3cretvalue" }
    def to_s = "s3cretvalue"
  end

  # An object that is nothing but its to_s.
  class Chatty
    def to_s = "password=hunter2"
    def inspect = to_s
  end

  def extra_for(context)
    build(RuntimeError.new("x"), context: context)[:extra]
  end

  def test_hash_like_context_values_are_scrubbed_by_key
    require "ostruct"
    creds = Struct.new(:user, :password)
    point = Data.define(:token, :page)
    extra = extra_for(
      params: FakeParams.new({ "user" => { "password" => "hunter2", "name" => "ann" } }),
      creds: creds.new("ann", "hunter2"),
      point: point.new(token: "hunter2", page: 2),
      ostruct: OpenStruct.new(api_key: "hunter2", ok: 1),
    )
    refute_includes extra.to_json, "hunter2"
    assert_equal({ "user" => { "password" => "[FILTERED]", "name" => "ann" } }, extra[:params])
    assert_equal({ "user" => "ann", "password" => "[FILTERED]" }, extra[:creds])
    assert_equal({ "token" => "[FILTERED]", "page" => 2 }, extra[:point])
    assert_equal({ "api_key" => "[FILTERED]", "ok" => 1 }, extra[:ostruct])
  end

  def test_any_other_object_is_only_its_class_name
    extra = extra_for(chatty: Chatty.new, list: [Chatty.new], keyed: { Chatty.new => 1 }, anon: Class.new { def to_s = "hunter2" }.new)
    refute_includes extra.to_json, "hunter2"
    assert_equal "#<EventBuilderTest::Chatty>", extra[:chatty]
    assert_equal ["#<EventBuilderTest::Chatty>"], extra[:list]
    assert_equal({ "#<EventBuilderTest::Chatty>" => 1 }, extra[:keyed])
    assert_equal "#<anonymous>", extra[:anon]
  end

  # ENV has a to_h. Expanded, it would send every variable filtered only by
  # name: DATABASE_URL and FLARE_DSN among them.
  def test_env_and_other_to_h_objects_stay_opaque
    ENV["FLARE_TEST_DATABASE_URL"] = "postgres://app:s3cretvalue@db/app"
    extra = extra_for(env: ENV, vault: Vault.new, set: Set["s3cretvalue"])
    refute_includes extra.to_json, "s3cretvalue"
    assert_equal "#<Object>", extra[:env] # ENV is a bare Object
    assert_equal "#<EventBuilderTest::Vault>", extra[:vault]
    assert_equal "#<Set>", extra[:set]
  ensure
    ENV.delete("FLARE_TEST_DATABASE_URL")
  end

  # Credentials in a value, whatever key it sits under.
  def test_credentials_in_values_are_redacted
    extra = extra_for(
      note: "connect postgres://app:s3cretvalue@db:5432/app failed",
      dsn: "http://s3cretvalue@host.docker.internal:8080/ingest/1",
      kw: "host=db password=s3cretvalue sslmode=require",
      quoted: "dbname=x password='s3cret value'",
      list: ["redis://:s3cretvalue@cache:6379/0"],
      plain: "https://example.com/a@b and user@example.com",
    )
    refute_includes extra.to_json, "s3cret"
    assert_equal "connect postgres://[FILTERED]@db:5432/app failed", extra[:note]
    assert_equal "http://[FILTERED]@host.docker.internal:8080/ingest/1", extra[:dsn]
    assert_equal "host=db password=[FILTERED] sslmode=require", extra[:kw]
    assert_equal ["redis://[FILTERED]@cache:6379/0"], extra[:list]
    # An "@" after a scheme reads as userinfo, even in a path: redacting a
    # path segment is the cheap mistake; a password with "@" or "/" in it
    # leaking would not be.
    assert_equal "https://[FILTERED]@b and user@example.com", extra[:plain]
  end

  # Rails' rules for filter_parameters, in the URL and in the params alike:
  # a dotted filter matches the nested path, a Regexp the key itself.
  def test_filters_follow_rails_nested_key_rules
    c = config
    c.rails_filters = ["credit_card.number", /\Assn\z/]
    url = "https://a.lan/pay?credit_card[number]=4111&credit_card[name]=ann&u[ssn]=123&ssn_note=ok&card_number=9"
    event = Flare::EventBuilder.new(c).build(RuntimeError.new("x"), handled: true, severity: :error, source: nil,
      fingerprint: nil, context: { controller: Ctrl.new(Req.new(url, "POST",
        { "credit_card" => { "number" => "4111", "name" => "ann" }, "u" => { "ssn" => "123" }, "number" => "1" })) })
    assert_equal "https://a.lan/pay?credit_card[number]=[FILTERED]&credit_card[name]=ann&u[ssn]=[FILTERED]&ssn_note=ok&card_number=9",
                 event[:request][:url]
    assert_equal({ "credit_card" => { "number" => "[FILTERED]", "name" => "ann" }, "u" => { "ssn" => "[FILTERED]" }, "number" => "1" },
                 event[:request][:data])
  end

  # Where Rails offers filtered_path (its own ParameterFilter, procs and
  # all), that is what's filtered further, not the raw URL.
  def test_rails_filtered_path_is_preferred
    req = Struct.new(:url, :request_method, :filtered_parameters, :base_url, :filtered_path)
      .new("https://a.lan/p?pin=1234&token=t", "GET", {}, "https://a.lan", "/p?pin=[FILTERED]&token=t")
    assert_equal "https://a.lan/p?pin=[FILTERED]&token=[FILTERED]",
                 build(RuntimeError.new("x"), context: { controller: Ctrl.new(req) })[:request][:url]
  end

  # Nothing hash-like gets past the scrub nested inside a Hash either.
  def test_nested_hash_like_values_are_scrubbed
    extra = extra_for(outer: { inner: FakeParams.new({ "secret" => "hunter2" }) })
    assert_equal({ "inner" => { "secret" => "[FILTERED]" } }, extra[:outer])
  end

  # The reviewer's five, each of which leaked before.
  def test_credential_shapes_the_patterns_used_to_miss
    {
      "redis://:p@ss@cache:6379/0" => "redis://[FILTERED]@cache:6379/0",
      "postgres://u:p/ss@db/x" => "postgres://[FILTERED]@db/x",
      '{"password":"hunter2","user":"ann"}' => '{"password":"[FILTERED]","user":"ann"}',
      "Authorization: Bearer abc.def-ghi" => "Authorization: [FILTERED]",
      "GET /x?api_key=k123&page=2" => "GET /x?api_key=[FILTERED]&page=2",
      "retry with Basic dXNlcjpwYXNz" => "retry with Basic [FILTERED]",
      "secret: 'two words'" => "secret: '[FILTERED]'",
    }.each do |raw, want|
      assert_equal want, extra_for(v: raw)[:v], raw
    end
  end

  # Redaction runs on the thread that hit the error: long hostile values
  # finish quickly (each pattern also has a time limit, past which the
  # whole string is dropped).
  def test_long_values_redact_quickly
    c = config
    started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    out = [c.redact("token=" + ("a" * 50_000)), c.redact("http://" + ("a:" * 20_000)), c.redact("x" * 100_000 + "@")]
    assert Process.clock_gettime(Process::CLOCK_MONOTONIC) - started < 1.0
    assert out[0].start_with?("token=[FILTERED]")
  end

  # :full sends messages, but not the credentials code interpolated into them.
  def test_exception_messages_are_redacted_causes_included
    error = begin
      begin
        raise IOError, "could not connect postgres://app:hunter2@db/prod"
      rescue IOError
        raise ArgumentError, "retry failed: Authorization: Bearer hunter2token"
      end
    rescue ArgumentError => e
      e
    end
    values = build(error)[:exception][:values]
    refute_includes values.to_json, "hunter2"
    assert_equal ["could not connect postgres://[FILTERED]@db/prod", "retry failed: Authorization: [FILTERED]"],
                 values.map { |v| v[:value] }
  end

  # A credential straddling the message cut is still found: redaction reads
  # past it.
  def test_a_credential_at_the_cut_is_redacted
    msg = ("x" * (Flare::EventBuilder::MAX_MESSAGE - 20)) + " postgres://app:hunter2hunter2@db/prod"
    value = build(RuntimeError.new(msg))[:exception][:values].last[:value]
    assert_equal Flare::EventBuilder::MAX_MESSAGE, value.length
    refute_includes value, "hunter2"
  end

  # Ruby's own inspect output: Ruby 3.4+ ({"k" => v}, {k: v}) and older
  # ({"k"=>v}, {:k=>v}), in a value.
  def test_ruby_inspect_shapes_are_redacted
    {
      { "password" => "hunter2", "user" => "ann" }.inspect => '{"password" => "[FILTERED]", "user" => "ann"}',
      { password: "hunter2" }.inspect => '{password: "[FILTERED]"}',
      '{"password"=>"hunter2"}' => '{"password"=>"[FILTERED]"}',
      '{:password=>"hunter2"}' => '{:password=>"[FILTERED]"}',
      '{:api_key => "hunter2"}' => '{:api_key => "[FILTERED]"}',
      '{"token" => ["hunter2", "b"]}' => '{"token" => [FILTERED]}',
    }.each do |raw, want|
      assert_equal want, extra_for(v: raw)[:v], raw
    end
  end

  # "-" and "_" read alike, and private keys count.
  def test_hyphenated_and_private_keys_are_redacted
    {
      "X-Api-Key: hunter2 rest" => "X-Api-Key: [FILTERED] rest",
      "api-key=hunter2&page=2" => "api-key=[FILTERED]&page=2",
      "apikey=hunter2" => "apikey=[FILTERED]",
      '{"private_key" => "hunter2"}' => '{"private_key" => "[FILTERED]"}',
      "private-key: hunter2" => "private-key: [FILTERED]",
      "X-Access-Key=hunter2" => "X-Access-Key=[FILTERED]",
    }.each do |raw, want|
      assert_equal want, extra_for(v: raw)[:v], raw
    end
    # And as keys, not only inside values.
    extra = extra_for("X-Api-Key" => "hunter2", "private-key" => "hunter2", "Private_Key" => "hunter2", "page" => 2)
    refute_includes extra.to_json, "hunter2"
    assert_equal 2, extra["page"]
  end

  # The reviewer's case end to end: a hash interpolated into a raise. The
  # message is redacted, and so are the source lines sent with the frame,
  # where the literal sits.
  def test_a_raise_interpolating_a_hash_sends_no_value
    error = begin
      params = { "password" => "hunter2", "api-key" => "hunter2", user: "ann" }
      raise ArgumentError, "bad params #{params}"
    rescue ArgumentError => e
      e
    end
    event = build(error)
    refute_includes event.to_json, "hunter2"
    value = event[:exception][:values].last
    assert_equal 'bad params {"password" => "[FILTERED]", "api-key" => "[FILTERED]", user: "ann"}', value[:value]
    frame = value[:stacktrace][:frames].last
    assert_includes (frame[:pre_context] + [frame[:context_line]]).join("\n"), '"password" => "[FILTERED]"'
  end

  # A sensitive key's value can be a whole structure: it goes whole.
  def test_nested_values_under_a_sensitive_key
    {
      { auth: { user: "ann", pass: "hunter2" }, page: 2 }.inspect => "{auth: [FILTERED], page: 2}",
      { password: { value: "hunter2" } }.inspect => "{password: [FILTERED]}",
      '{"token" => [["hunter2"], {"b" => "hunter2"}], "ok" => 1}' => '{"token" => [FILTERED], "ok" => 1}',
      '{"secret":{"a":["hunter2",{"b":"}"}]},"n":1}' => '{"secret":[FILTERED],"n":1}',
      # Cut off before it closes: the rest of the line goes.
      '{password: {value: "hunter2", more: ' => "{password: [FILTERED]",
    }.each do |raw, want|
      assert_equal want, extra_for(v: raw)[:v], raw
    end
  end

  # An object's inspect under a sensitive name: the whole #<…> goes.
  def test_object_inspect_values_under_a_sensitive_key
    secret = Class.new do
      def initialize(v) = @value = v
      def inspect = "#<Secret value=#{@value.inspect}>"
    end
    holder = Class.new do
      def initialize(s) = (@name = "ann"; @password = s)
      def inspect = "#<User name=#{@name.inspect}, @password=#{@password.inspect}>"
    end
    {
      holder.new(secret.new("hunter2")).inspect => '#<User name="ann", @password=[FILTERED]>',
      { password: secret.new("hunter2"), x: 1 }.inspect => "{password: [FILTERED], x: 1}",
      '{:password=>#<Secret "hunter2">, :x=>1}' => "{:password=>[FILTERED], :x=>1}",
      '@api_key=#<Wrapped #<Inner "hunter2">> next' => "@api_key=[FILTERED] next",
    }.each do |raw, want|
      assert_equal want, extra_for(v: raw)[:v], raw
    end
  end

  # Every cookie in the header, not only the first.
  def test_cookie_headers_go_whole
    {
      "Cookie: sid=hunter2; other=hunter2\nAccept: */*" => "Cookie: [FILTERED]\nAccept: */*",
      "Set-Cookie: remember=hunter2; Path=/; HttpOnly" => "Set-Cookie: [FILTERED]",
      "cookie:a=hunter2;b=hunter2" => "cookie:[FILTERED]",
    }.each do |raw, want|
      assert_equal want, extra_for(v: raw)[:v], raw
    end
  end

  # Redact, then cut: a password long enough to run past any window the
  # message is cut to still loses every character.
  def test_redaction_runs_before_the_message_is_cut
    msg = "#{"x" * (Flare::EventBuilder::MAX_MESSAGE - 20)} postgres://app:#{"hunter2" * 1000}@db/prod"
    value = build(RuntimeError.new(msg))[:exception][:values].last[:value]
    refute_includes value, "hunter2"
    assert_equal Flare::EventBuilder::MAX_MESSAGE, value.length
  end
end
