# frozen_string_literal: true

require_relative "test_helper"

class ConfigTest < Minitest::Test
  def test_dsn_parsing_with_path_prefix
    c = Flare::Config.new
    c.dsn = "http://abc123@host.docker.internal:8080/ingest/4"
    parsed = c.parsed_dsn
    assert_equal "abc123", parsed.key
    # The /ingest prefix survives into the endpoint — it's what keeps the
    # route outside fog's /api middleware.
    assert_equal "http://host.docker.internal:8080/ingest/api/4/envelope/", parsed.endpoint
    assert c.enabled?
  end

  def test_dsn_parsing_without_prefix
    c = Flare::Config.new
    c.dsn = "http://k@fog.lan:8080/7"
    assert_equal "http://fog.lan:8080/api/7/envelope/", c.parsed_dsn.endpoint
  end

  def test_bad_dsns_disable
    ["", nil, "not a url", "http://fog.lan:8080/4", "http://k@fog.lan:8080/notanumber"].each do |dsn|
      c = Flare::Config.new
      c.dsn = dsn
      refute c.enabled?, "#{dsn.inspect} should disable flare"
    end
  end

  def test_dsn_set_after_enabled_check_takes_effect
    c = Flare::Config.new
    c.dsn = nil
    refute c.enabled? # memoizes the nil parse
    c.dsn = "http://k@fog.lan:8080/ingest/9"
    assert c.enabled?, "a DSN assigned after enabled? was first asked must re-enable flare"
    assert_equal "http://fog.lan:8080/ingest/api/9/envelope/", c.parsed_dsn.endpoint
  end

  def test_configure_dsn_after_railtie_style_enabled_check
    # The railtie asks Flare.enabled? at initializer time, before app
    # initializers run Flare.configure. The later assignment must win.
    Flare.reset!
    Flare.config.dsn = nil
    refute Flare.enabled?
    Flare.configure { |c| c.dsn = "http://k@fog:8080/ingest/1" }
    assert Flare.enabled?
  ensure
    Flare.reset!
  end

  def test_release_prefers_env
    old = ENV["GIT_SHA"]
    ENV["GIT_SHA"] = "abc123def"
    c = Flare::Config.new
    assert_equal "abc123def", c.release
  ensure
    old.nil? ? ENV.delete("GIT_SHA") : ENV["GIT_SHA"] = old
  end

  def test_release_is_computed_once
    old = ENV["GIT_SHA"]
    ENV["GIT_SHA"] = "first"
    c = Flare::Config.new
    assert_equal "first", c.release
    # Events are built per error; the release can't change inside a
    # process, so the first answer sticks — no per-event ENV reads.
    ENV["GIT_SHA"] = "second"
    assert_equal "first", c.release
    # An explicit assignment still wins.
    c.release = "override"
    assert_equal "override", c.release
  ensure
    old.nil? ? ENV.delete("GIT_SHA") : ENV["GIT_SHA"] = old
  end

  def test_scrub_filters_sensitive_keys_deeply
    c = Flare::Config.new
    scrubbed = c.scrub({
      "password" => "hunter2",
      "user" => { api_key: "k", name: "dave" },
      "list" => [{ "session_token" => "x" }],
      "fine" => "keep",
    })
    assert_equal Flare::Config::FILTERED, scrubbed["password"]
    assert_equal Flare::Config::FILTERED, scrubbed["user"][:api_key]
    assert_equal "dave", scrubbed["user"][:name]
    assert_equal Flare::Config::FILTERED, scrubbed["list"][0]["session_token"]
    assert_equal "keep", scrubbed["fine"]
  end

  def test_scrub_honors_rails_filters
    c = Flare::Config.new
    c.rails_filters = [:ssn, /card/]
    scrubbed = c.scrub({ "ssn" => "123", "card_number" => "4111", "ok" => "x" })
    assert_equal Flare::Config::FILTERED, scrubbed["ssn"]
    assert_equal Flare::Config::FILTERED, scrubbed["card_number"]
    assert_equal "x", scrubbed["ok"]
  end
end
