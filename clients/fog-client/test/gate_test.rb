# frozen_string_literal: true

require "minitest/autorun"
require "json"
require_relative "../lib/fog_client"
require_relative "../lib/fog_client/signin/gate"

# The gate's Rack half: identity comes from a verified Fog-Assertion, never
# from bare Remote-* headers, which any container on the network can send.
class GateTest < Minitest::Test
  B64 = FogClient::Signin::B64
  ISS = "https://fog.home.test"
  API = "http://fogd.internal:8080"
  AUD = "https://sab.home.test"

  class Fog
    attr_accessor :jwks

    def initialize(jwks) = @jwks = jwks

    def get_json(url, bearer: nil)
      case url
      when "#{API}/app/v1/gate"
        raise FogClient::Signin::Denied, "http_401" unless bearer == "sab-token"

        { "issuer" => ISS, "audiences" => [AUD], "jwksPath" => "/oauth2/jwks" }
      # No discovery: the gate never reads it (fog's discovery always names
      # its https sign-in issuer, whatever the gate's scheme).
      when "#{API}/oauth2/jwks" then @jwks
      else raise FogClient::Signin::Error, "no route #{url}"
      end
    end
  end

  class Log
    attr_reader :lines

    def initialize = @lines = []
    def warn(msg) = @lines << msg
  end

  def setup
    @now = Time.at(1_800_000_000)
    @key = OpenSSL::PKey::EC.generate("prime256v1")
    pt = @key.public_key.to_octet_string(:uncompressed)
    jwk = { "kty" => "EC", "crv" => "P-256", "kid" => "k1", "use" => "sig", "x" => B64.encode(pt[1, 32]), "y" => B64.encode(pt[33, 32]) }
    @fog = Fog.new({ "keys" => [jwk] })
    @verifier = FogClient::Signin::Gate::Verifier.new(api_url: API, api_token: "sab-token", http: @fog, clock: -> { @now })
    @log = Log.new
  end

  def assertion(extra = {})
    claims = { "iss" => ISS, "aud" => AUD, "sub" => "person:I/U", "preferred_username" => "kim", "name" => "Kim",
               "roles" => ["member"], "sid" => "s1", "iat" => @now.to_i, "exp" => @now.to_i + 60 }.merge(extra)
    h = B64.encode(JSON.generate("alg" => "ES256", "kid" => "k1", "typ" => "JWT"))
    p = B64.encode(JSON.generate(claims))
    seq = OpenSSL::ASN1.decode(@key.sign("SHA256", "#{h}.#{p}")).value
    "#{h}.#{p}.#{B64.encode(seq.map { |i| i.value.to_s(2).rjust(32, "\x00") }.join)}"
  end

  # through runs one request through the middleware and returns the env the
  # app saw.
  def through(headers, trust = nil)
    seen = nil
    app = ->(env) { seen = env; [200, {}, []] }
    FogClient::Signin::Gate::Middleware.new(app, verifier: @verifier, logger: @log, trust_remote_headers: trust).call(headers.dup)
    seen
  end

  def test_a_verified_assertion_is_who_passed
    env = through("HTTP_FOG_ASSERTION" => assertion, "HTTP_REMOTE_USER" => "mallory")
    who = env["fog.gate.identity"]
    assert_equal "kim", who.handle
    assert_equal "member", who.role
    assert_equal "kim", env["HTTP_REMOTE_USER"], "Remote-User is rewritten from the assertion"
    assert_equal "member", env["HTTP_REMOTE_GROUPS"]
  end

  # A container on the network sends Remote-User and no assertion.
  def test_a_forged_remote_user_without_an_assertion_is_nobody
    env = through("HTTP_REMOTE_USER" => "admin", "HTTP_REMOTE_GROUPS" => "operator", "HTTP_REMOTE_NAME" => "Admin")
    assert_nil env["fog.gate.identity"]
    FogClient::Signin::Gate::REMOTE_HEADERS.each { |h| refute env.key?(h), "#{h} reached the app" }
  end

  # Every Remote-* and Fog-* a client sent, in any spelling, is gone —
  # not only the four the gate sets.
  def test_every_remote_and_fog_header_is_deleted
    forged = { "HTTP_REMOTE_ROLE" => "admin", "HTTP_REMOTE_EMAIL" => "a@b", "HTTP_REMOTEUSER" => "admin",
               "HTTP_FOG_ASSERTION" => "x.y.z", "HTTP_FOG_PERSON" => "admin", "HTTP_FOG_ROLE" => "operator" }
    env = through(forged)
    forged.each_key { |h| refute env.key?(h), "#{h} reached the app" }
    env = through("HTTP_FOG_ASSERTION" => assertion, "HTTP_REMOTE_ROLE" => "admin", "HTTP_FOG_ROLE" => "operator")
    assert_equal "kim", env["fog.gate.identity"].handle
    refute env.key?("HTTP_REMOTE_ROLE")
    refute env.key?("HTTP_FOG_ROLE")
  end

  # An assertion fog signed for another app's host.
  def test_an_assertion_for_another_app_is_refused
    env = through("HTTP_FOG_ASSERTION" => assertion("aud" => "https://evil.home.test"), "HTTP_REMOTE_USER" => "kim")
    assert_nil env["fog.gate.identity"]
    refute env.key?("HTTP_REMOTE_USER")
  end

  def test_an_expired_assertion_is_refused
    old = assertion("iat" => @now.to_i - 300, "exp" => @now.to_i - 240)
    assert_nil through("HTTP_FOG_ASSERTION" => old)["fog.gate.identity"]
  end

  def test_a_tampered_or_foreign_assertion_is_refused
    good = assertion
    assert_nil through("HTTP_FOG_ASSERTION" => good[0..-5] + "AAAA")["fog.gate.identity"]
    assert_nil through("HTTP_FOG_ASSERTION" => assertion("iss" => "https://fog.elsewhere.test"))["fog.gate.identity"]
    other = OpenSSL::PKey::EC.generate("prime256v1")
    @key = other
    assert_nil through("HTTP_FOG_ASSERTION" => assertion)["fog.gate.identity"], "signed by a key fog doesn't publish"
  end

  def test_without_fogs_api_everyone_is_anonymous
    env = nil
    app = ->(e) { env = e; [200, {}, []] }
    FogClient::Signin::Gate::Middleware.new(app, logger: @log, env: {}).call("HTTP_REMOTE_USER" => "admin")
    assert_nil env["fog.gate.identity"]
    refute env.key?("HTTP_REMOTE_USER")
    assert(@log.lines.any? { |l| l.include?("FOG_API_URL") })
  end

  # The opt-out keeps Remote-* as they came, and says what that costs.
  def test_the_opt_out_trusts_headers_and_warns
    env = through({ "HTTP_REMOTE_USER" => "admin" }, true)
    assert_equal "admin", env["HTTP_REMOTE_USER"]
    assert(@log.lines.any? { |l| l.include?("trusting Remote-*") })
  end
end

# What fog advertises at /app/v1/gate is exactly what it signs, scheme
# included: an https setup and a plain-http one both verify without the
# opt-out, and an assertion naming the other scheme is refused.
class GateSchemeTest < GateTest
  def fog_for(scheme)
    jwks = @fog.jwks
    Class.new do
      define_method(:get_json) do |url, bearer: nil|
        case url
        when "#{API}/app/v1/gate" then { "issuer" => "#{scheme}://fog.home.test", "audiences" => ["#{scheme}://sab.home.test"], "jwksPath" => "/oauth2/jwks" }
        when "#{API}/oauth2/jwks" then jwks
        else raise FogClient::Signin::Error, "no route #{url}"
        end
      end
    end.new
  end

  def test_both_setups_verify_and_refuse_the_other_scheme
    { "https" => "http", "http" => "https" }.each do |scheme, other|
      @verifier = FogClient::Signin::Gate::Verifier.new(api_url: API, api_token: "sab-token", http: fog_for(scheme), clock: -> { @now })
      own = assertion("iss" => "#{scheme}://fog.home.test", "aud" => "#{scheme}://sab.home.test")
      assert_equal "kim", through("HTTP_FOG_ASSERTION" => own)["fog.gate.identity"]&.handle, "#{scheme} setup"
      [{ "aud" => "#{other}://sab.home.test" }, { "iss" => "#{other}://fog.home.test" }].each do |cross|
        bad = assertion({ "iss" => "#{scheme}://fog.home.test", "aud" => "#{scheme}://sab.home.test" }.merge(cross))
        assert_nil through("HTTP_FOG_ASSERTION" => bad)["fog.gate.identity"], "#{scheme} setup took #{cross}"
      end
    end
  end
end
