# frozen_string_literal: true

require "minitest/autorun"
require "json"
require_relative "../lib/fog_client"

# The Go side signs, Ruby verifies: test/vectors/signin.json is written by
# fog's internal/signing tests from a real Keyring and checked in, so the two
# languages are held to the same tokens (as the consent grammar's vectors
# hold its two implementations).
class SigninVectorsTest < Minitest::Test
  VECTORS = JSON.parse(File.read(File.join(__dir__, "vectors", "signin.json")))
  NOW = Time.at(VECTORS["now"])

  class StaticHttp
    def initialize(routes) = @routes = routes
    def get_json(url, bearer: nil) = @routes.fetch(url) { raise FogClient::Signin::Error, "no route #{url}" }
    def post_form(url, form, basic: nil) = @routes.fetch(url).call(form, basic)
  end

  def config
    FogClient::Signin::Config.new(issuer: VECTORS["issuer"], client_id: VECTORS["client_id"], client_secret: "s3cret")
  end

  def provider(clock: -> { NOW })
    http = StaticHttp.new(
      "#{VECTORS['issuer']}/.well-known/openid-configuration" => { "issuer" => VECTORS["issuer"], "jwks_uri" => "#{VECTORS['issuer']}/oauth2/jwks" },
      "#{VECTORS['issuer']}/oauth2/jwks" => VECTORS["jwks"],
    )
    FogClient::Signin::Provider.new(config, http: http, clock: clock)
  end

  def test_every_vector
    flow = FogClient::Signin::Flow.new(provider, redirect_uri: "https://stream.home.test/cb", clock: -> { NOW })
    VECTORS["id_tokens"].each do |v|
      if v["valid"]
        identity = flow.verify_id_token(v["token"], nonce: v["nonce"])
        assert_equal v["sub"], identity.subject, v["name"]
        assert_equal v["roles"], identity.roles, v["name"]
        assert_equal v["visitor"] == true, identity.visitor?, v["name"]
        # The app's session never outlives fog's: the stated expiry, else
        # the cap counted from now.
        cap = NOW.to_i + (identity.visitor? ? FogClient::Signin::VISITOR_CAP : FogClient::Signin::SESSION_CAP)
        assert_equal [v["session_exp"] || cap, cap].min, identity.session_exp, v["name"]
        assert_equal Time.at(identity.session_exp), identity.expires_at, v["name"]
      else
        assert_raises(FogClient::Signin::Invalid, v["name"]) { flow.verify_id_token(v["token"], nonce: v["nonce"]) }
      end
    end
  end

  def test_logout_tokens
    bc = FogClient::Signin::Backchannel.new(provider, clock: -> { NOW })
    VECTORS["logout_tokens"].each do |v|
      if v["valid"]
        out = bc.verify(v["token"])
        assert_equal v["sid"].to_s, out.sid.to_s, v["name"]
        if v["all_before"]
          assert_equal Time.at(v["all_before"]), out.all_before, v["name"]
          assert out.all?, v["name"]
        else
          refute out.all?, v["name"]
        end
        assert_equal v["sub"], out.subject, v["name"] if v["sub"]
        assert_raises(FogClient::Signin::Invalid, "#{v['name']} replayed") { bc.verify(v["token"]) }
      else
        assert_raises(FogClient::Signin::Invalid, v["name"]) { bc.verify(v["token"]) }
      end
    end
  end

  def test_proofs
    refute_empty VECTORS["proofs"]
    VECTORS["proofs"].each do |v|
      assert_equal v["valid"] == true, FogClient::Signin::Proof.verify(v["token"], provider, clock: -> { NOW }), v["name"]
    end
  end

  def test_a_sent_nonce_is_never_optional
    flow = FogClient::Signin::Flow.new(provider, redirect_uri: "https://stream.home.test/cb", clock: -> { NOW })
    valid = VECTORS["id_tokens"].find { |v| v["name"] == "valid" }
    assert_raises(FogClient::Signin::Invalid) { flow.verify_id_token(valid["token"], nonce: "") }
    assert_raises(FogClient::Signin::Invalid, "a nonce where none was sent") { flow.verify_id_token(valid["token"], nonce: nil) }
  end
end

class SigninStateTest < Minitest::Test
  CFG = FogClient::Signin::Config.new(issuer: "https://fog.home.test", client_id: "stream", client_secret: "s3cret")

  def test_seal_round_trip_and_tamper
    sealed = FogClient::Signin::State.seal(CFG, { "n" => "x" })
    assert_equal({ "n" => "x" }, FogClient::Signin::State.open(CFG, sealed))
    tampered = sealed.dup
    tampered[-2] = tampered[-2] == "A" ? "B" : "A"
    assert_raises(FogClient::Signin::Invalid) { FogClient::Signin::State.open(CFG, tampered) }
    other = FogClient::Signin::Config.new(issuer: CFG.issuer, client_id: "stream", client_secret: "other")
    assert_raises(FogClient::Signin::Invalid) { FogClient::Signin::State.open(other, sealed) }
  end

  def test_a_sealed_link_is_not_a_state
    pending = FogClient::Signin::State.seal(CFG, { "identity" => {} }, purpose: "pending")
    assert_raises(FogClient::Signin::Invalid) { FogClient::Signin::State.open(CFG, pending) }
    assert_equal({ "identity" => {} }, FogClient::Signin::State.open(CFG, pending, purpose: "pending"))
  end
end

class SigninFlowTest < Minitest::Test
  CFG = FogClient::Signin::Config.new(issuer: "https://fog.home.test", client_id: "stream", client_secret: "s 3cret&")

  class Recorder
    attr_reader :posts

    def initialize = @posts = []

    def get_json(url, bearer: nil)
      { "issuer" => "https://fog.home.test", "authorization_endpoint" => "https://fog.home.test/oauth2/authorize",
        "pushed_authorization_request_endpoint" => "https://fog.home.test/oauth2/par",
        "token_endpoint" => "https://fog.home.test/oauth2/token", "jwks_uri" => "https://fog.home.test/oauth2/jwks" }
    end

    def post_form(url, form, basic: nil)
      @posts << [url, form, basic]
      { "request_uri" => "urn:ietf:params:oauth:request_uri:abc" }
    end
  end

  def test_start_pushes_and_binds
    http = Recorder.new
    provider = FogClient::Signin::Provider.new(CFG, http: http)
    start = FogClient::Signin::Flow.new(provider, redirect_uri: "https://stream.home.test/cb", http: http).start(return_to: "/x")
    url, form, basic = http.posts.first
    assert_equal "https://fog.home.test/oauth2/par", url
    assert_equal "S256", form["code_challenge_method"]
    assert_equal ["stream", "s 3cret&"], basic
    assert_match %r{\Ahttps://fog\.home\.test/oauth2/authorize\?client_id=stream&request_uri=}, start.url
    refute_includes start.url, "code_challenge", "PAR keeps the request off the URL"
    refute_empty start.binding
  end

  def test_finish_refuses_another_browser_and_mixups
    http = Recorder.new
    provider = FogClient::Signin::Provider.new(CFG, http: http)
    flow = FogClient::Signin::Flow.new(provider, redirect_uri: "https://stream.home.test/cb", http: http)
    flow.start
    state = http.posts.first[1]["state"]
    assert_raises(FogClient::Signin::Invalid) { flow.finish({ "state" => state, "code" => "c" }, binding: "someone-else") }
    assert_raises(FogClient::Signin::Invalid) { flow.finish({ "state" => state, "code" => "c", "iss" => "https://evil.test" }, binding: nil) }
  end

  class PromisesIss < Recorder
    def get_json(url, bearer: nil) = super.merge("authorization_response_iss_parameter_supported" => true)
  end

  def test_finish_requires_iss_when_promised
    http = PromisesIss.new
    provider = FogClient::Signin::Provider.new(CFG, http: http)
    flow = FogClient::Signin::Flow.new(provider, redirect_uri: "https://stream.home.test/cb", http: http)
    start = flow.start
    state = http.posts.first[1]["state"]
    e = assert_raises(FogClient::Signin::Invalid) { flow.finish({ "state" => state, "code" => "c" }, binding: start.binding) }
    assert_match(/where it came from/, e.message)
  end

  def test_internal_url_rewrites_back_channel_only
    cfg = FogClient::Signin::Config.new(issuer: "https://fog.home.test", client_id: "s", client_secret: "x", internal_url: "http://fogd:8080")
    assert_equal "http://fogd:8080/oauth2/token", cfg.back_channel("https://fog.home.test/oauth2/token")
  end

  def test_config_is_nil_off_fog
    assert_nil FogClient::Signin.config({})
    refute FogClient::Signin.enabled?({ "OIDC_ISSUER" => "x" })
  end
end

class LinkerTest < Minitest::Test
  Adapter = Struct.new(:by_identity, :by_external, :count) do
    def find_by_identity(_iss, _sub) = by_identity
    def find_by_external_id(_sub) = by_external
    def account_count = count
  end
  ID = FogClient::Signin::Identity.new(issuer: "i", subject: "s", roles: ["admin"], email: "victim@example.com")

  def decide(adapter, roles: ["admin"])
    FogClient::Signin::Linker.resolve(ID.dup.tap { |i| i.roles = roles }, adapter, admin_role: "admin").action
  end

  def test_decisions
    assert_equal :sign_in, decide(Adapter.new(:acct, nil, 3))
    assert_equal :adopt, decide(Adapter.new(nil, :acct, 3))
    assert_equal :bootstrap, decide(Adapter.new(nil, nil, 0))
    assert_equal :provision, decide(Adapter.new(nil, nil, 0), roles: ["member"])
    assert_equal :link, decide(Adapter.new(nil, nil, 2)), "never matched by email, even when one would match"
  end

  def test_visitors_are_never_this_households_people
    visitor = ID.dup.tap { |i| i.visitor = true }
    resolve = ->(adapter) { FogClient::Signin::Linker.resolve(visitor, adapter, admin_role: "admin").action }
    assert_equal :sign_in, resolve.(Adapter.new(:acct, nil, 3))
    assert_equal :visit, resolve.(Adapter.new(nil, :acct, 3)), "never adopt a provisioned account"
    assert_equal :visit, resolve.(Adapter.new(nil, nil, 0)), "never bootstrap the app's admin"
    assert_equal :visit, resolve.(Adapter.new(nil, nil, 2)), "never link an existing account"
  end

  class Hosts < Adapter
    def visitor_account?(account) = account == :visitor_acct
  end

  def test_visitor_and_account_must_agree
    visitor = ID.dup.tap { |i| i.visitor = true }
    resolve = ->(identity, adapter) { FogClient::Signin::Linker.resolve(identity, adapter, admin_role: "admin").action }
    assert_equal :sign_in, resolve.(visitor, Hosts.new(:visitor_acct, nil, 3))
    assert_equal :refuse, resolve.(visitor, Hosts.new(:household_acct, nil, 3))
    assert_equal :refuse, resolve.(ID, Hosts.new(:visitor_acct, nil, 3))
  end
end

class DeviceTest < Minitest::Test
  CFG = FogClient::Signin::Config.new(issuer: "https://fog.home.test", client_id: "stream", client_secret: "s3cret")

  class Scripted
    attr_reader :posts

    def initialize(answers)
      @answers = answers
      @posts = []
    end

    def get_json(_url, bearer: nil)
      { "issuer" => "https://fog.home.test", "device_authorization_endpoint" => "https://fog.home.test/oauth2/device_authorization",
        "token_endpoint" => "https://fog.home.test/oauth2/token", "jwks_uri" => "https://fog.home.test/oauth2/jwks" }
    end

    def post_form(url, form, basic: nil)
      @posts << [url, form, basic]
      a = @answers.shift
      raise a if a.is_a?(Exception)

      a
    end
  end

  def test_start_authenticates_and_names_the_device
    http = Scripted.new([{ "device_code" => "dc", "user_code" => "BCDF-GHJK", "verification_uri" => "https://fog.home.test/device",
                           "verification_uri_complete" => "https://fog.home.test/device?user_code=BCDF-GHJK", "expires_in" => 600, "interval" => 5 }])
    provider = FogClient::Signin::Provider.new(CFG, http: http)
    code = FogClient::Signin::Device.new(provider, http: http).start(device_name: "Living Room TV")
    assert_equal "BCDF-GHJK", code.user_code
    _, form, basic = http.posts.first
    assert_equal "Living Room TV", form["device_name"]
    assert_equal ["stream", "s3cret"], basic
  end

  def test_poll_pending_and_slow_down
    code = FogClient::Signin::Device::Code.new(device_code: "dc", interval: 5)
    http = Scripted.new([FogClient::Signin::Denied.new("authorization_pending"), FogClient::Signin::Denied.new("slow_down"),
                         FogClient::Signin::Denied.new("access_denied")])
    device = FogClient::Signin::Device.new(FogClient::Signin::Provider.new(CFG, http: http), http: http)
    e = assert_raises(FogClient::Signin::Device::Pending) { device.poll(code) }
    assert_equal 5, e.interval
    e = assert_raises(FogClient::Signin::Device::Pending) { device.poll(code) }
    assert_equal 10, e.interval
    assert_raises(FogClient::Signin::Denied) { device.poll(code) }
  end
end

class ProofThrottleTest < Minitest::Test
  def setup = FogClient::Signin::Proof.reset_throttle!

  # fogd unreachable: every call to it is counted.
  class Down
    attr_reader :calls

    def initialize = @calls = 0

    def get_json(*, **)
      @calls += 1
      raise SocketError, "connection refused"
    end
  end

  def test_a_failed_proof_is_not_asked_again_for_a_while
    env = { "FOG_API_URL" => "http://fogd", "FOG_API_TOKEN" => "t" }
    http = Down.new
    now = Time.at(1_000)
    refute FogClient::Signin::Proof.check_throttled(nil, env, http: http, clock: -> { now })
    refute FogClient::Signin::Proof.check_throttled(nil, env, http: http, clock: -> { now + 10 })
    assert_equal 1, http.calls
    refute FogClient::Signin::Proof.check_throttled(nil, env, http: http, clock: -> { now + 31 })
    assert_equal 2, http.calls
  end
end

# Tokens this test signs itself, with a key it makes: for what fog never
# sends (nbf, odd headers) and for key rotation.
class SigninHardeningTest < Minitest::Test
  B64 = FogClient::Signin::B64
  ISS = "https://fog.home.test"

  class Keys
    attr_accessor :set
    def get_json(url, bearer: nil)
      return { "issuer" => ISS, "jwks_uri" => "#{ISS}/oauth2/jwks" } if url.end_with?("openid-configuration")

      @set
    end
  end

  def key_pair(kid)
    k = OpenSSL::PKey::EC.generate("prime256v1")
    pt = k.public_key.to_octet_string(:uncompressed)
    [k, { "kty" => "EC", "crv" => "P-256", "kid" => kid, "use" => "sig", "x" => B64.encode(pt[1, 32]), "y" => B64.encode(pt[33, 32]) }]
  end

  def sign(key, kid, claims, header: nil)
    h = B64.encode(JSON.generate(header || { "alg" => "ES256", "kid" => kid, "typ" => "JWT" }))
    p = B64.encode(JSON.generate(claims))
    der = key.sign("SHA256", "#{h}.#{p}")
    seq = OpenSSL::ASN1.decode(der).value
    sig = seq.map { |i| i.value.to_s(2).rjust(32, "\x00") }.join
    "#{h}.#{p}.#{B64.encode(sig)}"
  end

  def setup
    @now = Time.at(1_800_000_000)
    @http = Keys.new
    @key, jwk = key_pair("k1")
    @http.set = { "keys" => [jwk] }
    cfg = FogClient::Signin::Config.new(issuer: ISS, client_id: "stream", client_secret: "s")
    @provider = FogClient::Signin::Provider.new(cfg, http: @http, clock: -> { @now })
    @flow = FogClient::Signin::Flow.new(@provider, redirect_uri: "https://stream.home.test/cb", clock: -> { @now })
  end

  def claims(extra = {})
    { "iss" => ISS, "aud" => "stream", "sub" => "person:I/U", "iat" => @now.to_i, "exp" => @now.to_i + 300, "nonce" => "n" }.merge(extra)
  end

  # Ruby 3.4 dropped base64 from the default gems: the client must load and
  # verify without it.
  def test_loads_without_the_base64_gem
    lib = File.expand_path("../lib", __dir__)
    out = IO.popen([RbConfig.ruby, "--disable-gems", "-I", lib, "-e",
      'begin; require "base64"; puts "base64 loadable"; rescue LoadError; end; require "fog_client"; ' \
      'b = FogClient::Signin::B64; s = (0..255).map(&:chr).join.b; puts b.decode(b.encode(s)) == s'],
      err: [:child, :out], &:read)
    assert_equal "true\n", out
  end

  def test_base64url_is_strict
    assert_equal "\xFF\xEE".b, B64.decode("_-4")
    ["a", "ab=", "a+b", "a/b", " ab", "ab\n"].each do |bad|
      assert_raises(FogClient::Signin::Invalid, bad.inspect) { B64.decode(bad) }
    end
  end

  # A header that is JSON but not an object was a TypeError, not a refusal.
  def test_a_header_that_is_not_an_object_is_invalid
    ["[1]", "1", "\"x\"", "null"].each do |h|
      tok = "#{B64.encode(h)}.#{B64.encode('{}')}.#{B64.encode('x' * 64)}"
      assert_raises(FogClient::Signin::Invalid, h) { @flow.verify_id_token(tok, nonce: "n") }
    end
    good_header = B64.encode(JSON.generate("alg" => "ES256", "kid" => "k1"))
    payload_not_object = "#{good_header}.#{B64.encode('[1]')}"
    sig = sign(@key, "k1", {}).split(".")[2]
    assert_raises(FogClient::Signin::Invalid) { @flow.verify_id_token("#{payload_not_object}.#{sig}", nonce: "n") }
  end

  # nbf was never checked.
  def test_not_before_is_checked
    assert @flow.verify_id_token(sign(@key, "k1", claims("nbf" => @now.to_i)), nonce: "n")
    assert_raises(FogClient::Signin::Invalid) { @flow.verify_id_token(sign(@key, "k1", claims("nbf" => @now.to_i + 3600)), nonce: "n") }
    assert_raises(FogClient::Signin::Invalid) { @flow.verify_id_token(sign(@key, "k1", claims("nbf" => "soon")), nonce: "n") }
  end

  # fog's emergency rotation retires every key: a key the client already
  # trusted stops verifying once the provider stops publishing it.
  def test_a_retired_key_stops_verifying
    old = sign(@key, "k1", claims)
    assert @flow.verify_id_token(old, nonce: "n")
    _, fresh = key_pair("k2")
    @http.set = { "keys" => [fresh] }
    @now += FogClient::Signin::Provider::KEYS_TTL + 1
    assert_raises(FogClient::Signin::Invalid) { @flow.verify_id_token(sign(@key, "k1", claims), nonce: "n") }
  end

  # The app's session is capped at fog's, and at 30 days (12 hours for a
  # visitor) when the provider states nothing or something later.
  def test_the_session_cap
    soon = @now.to_i + 3600
    assert_equal soon, @flow.verify_id_token(sign(@key, "k1", claims("fog_session_exp" => soon)), nonce: "n").session_exp
    far = @now.to_i + (90 * 24 * 3600)
    assert_equal @now.to_i + FogClient::Signin::SESSION_CAP, @flow.verify_id_token(sign(@key, "k1", claims("fog_session_exp" => far)), nonce: "n").session_exp
    visitor = @flow.verify_id_token(sign(@key, "k1", claims("fog_visitor" => true)), nonce: "n")
    assert_equal @now.to_i + FogClient::Signin::VISITOR_CAP, visitor.session_exp
  end

  # A browser drops tabs and newlines and reads a backslash as a slash, so
  # these become another origin; every one must come back as "/".
  def test_return_to_stays_on_this_host
    ["/\t/evil.example", "/\n/evil.example", "/\r/evil.example", "/\\evil.example", "\\/evil.example",
     "//evil.example", "https://evil.example/", "/\x00x", "javascript:alert(1)", "", nil, "evil", "/\u0007/x"].each do |bad|
      assert_equal "/", FogClient::Signin.safe_return_to(bad), bad.inspect
    end
    ["/", "/library?shelf=2#top", "/a/b%2Fc"].each do |good|
      assert_equal good, FogClient::Signin.safe_return_to(good), good
    end
  end

  def test_a_logout_token_past_its_exp_is_refused
    bc = FogClient::Signin::Backchannel.new(@provider, clock: -> { @now })
    lt = { "iss" => ISS, "aud" => "stream", "iat" => @now.to_i, "jti" => "j1", "sid" => "s",
           "events" => { FogClient::Signin::Backchannel::EVENT => {} } }
    assert bc.verify(sign(@key, "k1", lt.merge("exp" => @now.to_i + 120), header: { "alg" => "ES256", "kid" => "k1", "typ" => "logout+jwt" }))
    assert_raises(FogClient::Signin::Invalid) do
      bc.verify(sign(@key, "k1", lt.merge("jti" => "j2", "exp" => @now.to_i - 3600), header: { "alg" => "ES256", "kid" => "k1", "typ" => "logout+jwt" }))
    end
  end
end
