# frozen_string_literal: true

require_relative "test_helper"
require "json"
require "rbconfig"

# The reviewers' two findings against the real thing: actionpack's own
# ActionController::Parameters and ActionDispatch::Request#filtered_path,
# loaded in a child process (this suite runs without Rails) from an app's
# installed bundle. Skipped where no bundle with actionpack is checked out;
# point FLARE_RAILS_GEM_PATH at one to run it.
class RealRailsTest < Minitest::Test
  def gem_path
    abi = RbConfig::CONFIG["ruby_version"]
    [ENV["FLARE_RAILS_GEM_PATH"], File.expand_path("~/src/fit/vendor/bundle/ruby/#{abi}")]
      .compact.find { |d| Dir.glob(File.join(d, "gems", "actionpack-*")).any? }
  end

  SCRIPT = <<~'RUBY'
    require "action_controller"
    require "action_dispatch"
    require "rack/mock"
    require "flare"
    ENV["FLARE_TEST_DATABASE_URL"] = "postgres://app:s3cretvalue@db/app"
    filters = ["credit_card.number", /\Assn\z/, :password]
    env = Rack::MockRequest.env_for("https://fit.lan/pay?credit_card[number]=4111&u[ssn]=123&credit_card[name]=ann&page=2")
    env["action_dispatch.parameter_filter"] = filters
    request = ActionDispatch::Request.new(env)
    controller = Struct.new(:request) { def action_name = "pay" }.new(request)
    params = ActionController::Parameters.new("user" => { "password" => "hunter2", "name" => "ann" })
    c = Flare::Config.new
    c.dsn = "http://k@fog:8080/ingest/1"
    c.rails_filters = filters
    event = Flare::EventBuilder.new(c).build(RuntimeError.new("x"), handled: true, severity: :error, source: nil,
      fingerprint: nil, context: { controller: controller, params: params, env: ENV })
    print JSON.generate(url: event[:request][:url], params: event[:extra][:params], env: event[:extra][:env])
  RUBY

  def test_parameters_env_and_filtered_path_with_real_rails
    path = gem_path or skip "no bundle with actionpack checked out (set FLARE_RAILS_GEM_PATH)"
    lib = File.expand_path("../lib", __dir__)
    out = IO.popen({ "GEM_PATH" => path, "GEM_HOME" => path, "BUNDLE_GEMFILE" => nil, "RUBYOPT" => nil },
                   [RbConfig.ruby, "-I", lib, "-e", SCRIPT], err: [:child, :out], &:read)
    got = JSON.parse(out) rescue flunk("child failed:\n#{out}")
    refute_includes out, "hunter2"
    refute_includes out, "s3cretvalue"
    assert_equal({ "user" => { "password" => "[FILTERED]", "name" => "ann" } }, got["params"])
    assert_equal "#<Object>", got["env"]
    url = got["url"]
    refute_includes url, "4111"
    refute_includes url, "=123"
    assert_includes url, "ann"
    assert_includes url, "page=2"
  end
end
