# frozen_string_literal: true

# FogClient::Signin — the relying-party half of fog-signin/1
# (fog:docs/signin-contract.md).
#
# It is plain OpenID Connect: authorization code with PKCE, pushed
# authorization requests, ID tokens verified against the provider's keys,
# back-channel logout. Point OIDC_ISSUER / OIDC_CLIENT_ID /
# OIDC_CLIENT_SECRET at fog and fog fills them in; point them at Pocket ID
# or Authelia and the same code signs people in there. Without them,
# Signin.config is nil and an app shows only its own sign-in.
#
# Zero dependencies, like the rest of this gem — not even the base64 gem,
# which stopped being a default gem in Ruby 3.4: base64url is pack/unpack.
# JOSE is done with OpenSSL, for exactly one algorithm (ES256), which is all
# fog signs with. A token naming any other algorithm is refused before a key
# is looked at.
#
# The pieces are separate on purpose, and the framework glue (a Rails
# controller in each app) stays thin:
#
#   Config      what the environment says
#   Provider    discovery and keys, cached
#   JWT         verify a compact ES256 JWS against a provider's keys
#   Flow        start (PAR + PKCE) and finish (code exchange + ID token)
#   State       the sealed, browser-bound state the flow carries
#   Backchannel verify a logout token
#   Linker      what a verified identity means for the app's accounts
#   Proof       is this app provably running on fog? (fog-only mode)

require "json"
require "net/http"
require "openssl"
require "securerandom"
require "uri"

module FogClient
  module Signin
    class Error < StandardError; end
    # The person or the provider said no (an OAuth error on the callback).
    class Denied < Error
      attr_reader :code

      def initialize(code, description = nil)
        @code = code
        super(description.to_s.empty? ? code : "#{code}: #{description}")
      end
    end
    # Something did not verify. Never shown in detail to a browser.
    class Invalid < Error; end

    # Config is what the environment says. issuer, client_id and secret are
    # the generic OIDC trio; internal_url is fog's, and lets back-channel
    # calls go straight to fogd over the fog network.
    Config = Struct.new(:issuer, :client_id, :client_secret, :internal_url, keyword_init: true) do
      # back_channel rewrites a provider URL onto the internal address, when
      # there is one. Front-channel URLs (authorize, end_session) never are.
      def back_channel(url)
        return url if internal_url.to_s.empty?

        u = URI(url)
        base = URI(internal_url)
        u.scheme, u.host, u.port = base.scheme, base.host, base.port
        u.to_s
      end
    end

    def self.config(env = ENV)
      issuer, id, secret = env["OIDC_ISSUER"], env["OIDC_CLIENT_ID"], env["OIDC_CLIENT_SECRET"]
      return nil if [issuer, id, secret].any? { |v| v.to_s.empty? }

      Config.new(issuer: issuer.chomp("/"), client_id: id, client_secret: secret,
                 internal_url: env["FOG_OIDC_INTERNAL_URL"].to_s.empty? ? nil : env["FOG_OIDC_INTERNAL_URL"].chomp("/"))
    end

    def self.enabled?(env = ENV) = !config(env).nil?

    # Identity is a verified sign-in, as the app sees it. subject is stable
    # forever (for fog: person:<household>/<person>); handle and name may
    # change and are display only.
    #
    # A visitor (fog:docs/signin-contract.md) is a paired household's
    # person, signed in by their own fog: subject and home name their home,
    # and they are never one of this household's people.
    #
    # session_exp (unix seconds) is when the app's session for this sign-in
    # must end: never later than fog's own session (the ID token's
    # fog_session_exp), and never later than SESSION_CAP — VISITOR_CAP for a
    # visitor — after the sign-in, whatever the provider says or doesn't.
    # expires_at is the same as a Time.
    Identity = Struct.new(:issuer, :subject, :handle, :name, :email, :email_verified, :roles,
                          :acr, :amr, :sid, :auth_time, :home, :visitor, :id_token, :session_exp, keyword_init: true) do
      def role = Array(roles).first
      def visitor? = visitor == true
      def expires_at = session_exp && Time.at(session_exp)
    end

    # The longest an app's session from one sign-in may last: fog's own
    # session lifetime, and a visitor's.
    SESSION_CAP = 30 * 24 * 3600
    VISITOR_CAP = 12 * 3600

    # session_exp is Identity#session_exp for claims verified at now: the
    # provider's fog_session_exp when it states one, capped either way.
    def self.session_exp(claims, now)
      cap = now + (claims["fog_visitor"] == true ? VISITOR_CAP : SESSION_CAP)
      stated = claims["fog_session_exp"]
      stated.is_a?(Integer) ? [stated, cap].min : cap
    end

    # B64 is unpadded base64url (RFC 4648 §5), strict, without the base64 gem.
    module B64
      def self.decode(s)
        raise Invalid, "malformed base64url" unless s.is_a?(String) && s.match?(/\A[A-Za-z0-9_-]*\z/) && s.length % 4 != 1

        s.tr("-_", "+/").ljust((s.length + 3) / 4 * 4, "=").unpack1("m0")
      rescue ArgumentError
        raise Invalid, "malformed base64url"
      end

      def self.encode(bytes) = [bytes].pack("m0").tr("+/", "-_").delete("=")
    end

    # Provider: discovery and keys. Keys are refetched when a token names one
    # we have not seen — a rotation — at most every 30 seconds, and every
    # KEYS_TTL regardless, so a key the provider retired (fog's emergency
    # rotation) stops verifying here within minutes, not at the next restart.
    class Provider
      REFETCH_EVERY = 30
      KEYS_TTL = 300
      DISCOVERY_TTL = 3600

      attr_reader :config

      def initialize(config, http: Http.new, clock: -> { Time.now })
        @config, @http, @clock = config, http, clock
        @mutex = Mutex.new
        @discovery = nil
        @discovered_at = nil
        @keys = {}
        @keys_at = nil
      end

      def discovery
        @mutex.synchronize do
          if @discovery.nil? || @clock.call - @discovered_at > DISCOVERY_TTL
            doc = @http.get_json(@config.back_channel("#{@config.issuer}/.well-known/openid-configuration"))
            raise Invalid, "discovery names issuer #{doc['issuer'].inspect}, not #{@config.issuer}" unless doc["issuer"] == @config.issuer

            @discovery, @discovered_at = doc, @clock.call
          end
          @discovery
        end
      end

      def endpoint(name)
        url = discovery[name]
        raise Error, "the provider has no #{name}" if url.to_s.empty?

        url
      end

      def key(kid)
        @mutex.synchronize do
          fresh = @keys_at && @clock.call - @keys_at < KEYS_TTL
          return @keys[kid] if fresh && @keys.key?(kid)
          return nil if @keys_at && @clock.call - @keys_at < REFETCH_EVERY
        end
        set = @http.get_json(@config.back_channel(endpoint("jwks_uri")))
        @mutex.synchronize do
          @keys = Array(set["keys"]).each_with_object({}) do |jwk, h|
            next unless jwk["kty"] == "EC" && jwk["crv"] == "P-256" && jwk["kid"]
            next if jwk["use"] && jwk["use"] != "sig"

            h[jwk["kid"]] = JWT.ec_key(jwk)
          end
          @keys_at = @clock.call
          @keys[kid]
        end
      end
    end

    # JWT: ES256 compact JWS, verified. Nothing else is accepted.
    module JWT
      def self.ec_key(jwk)
        x, y = B64.decode(jwk["x"]), B64.decode(jwk["y"])
        raise Invalid, "bad EC key" unless x.bytesize == 32 && y.bytesize == 32

        point = OpenSSL::PKey::EC::Point.new(OpenSSL::PKey::EC::Group.new("prime256v1"), OpenSSL::BN.new("\x04#{x}#{y}", 2))
        asn1 = OpenSSL::ASN1::Sequence([
          OpenSSL::ASN1::Sequence([OpenSSL::ASN1::ObjectId("id-ecPublicKey"), OpenSSL::ASN1::ObjectId("prime256v1")]),
          OpenSSL::ASN1::BitString(point.to_octet_string(:uncompressed)),
        ])
        OpenSSL::PKey::EC.new(asn1.to_der)
      end

      # decode verifies and returns [header, claims]. The algorithm is read
      # from the header only to refuse it: ES256 is assumed, never chosen.
      def self.decode(compact, provider, typ: nil)
        parts = compact.to_s.split(".", -1)
        raise Invalid, "not a compact JWS" unless parts.length == 3

        header = JSON.parse(B64.decode(parts[0]))
        raise Invalid, "the header is not a JSON object" unless header.is_a?(Hash)
        raise Invalid, "unexpected algorithm #{header['alg'].inspect}" unless header["alg"] == "ES256"
        raise Invalid, "crit headers are not supported" if header.key?("crit")
        raise Invalid, "embedded keys are not trusted" if header.key?("jwk") || header.key?("jku") || header.key?("x5u")
        raise Invalid, "unexpected typ" if typ && header["typ"].to_s.downcase != typ

        key = provider.key(header["kid"].to_s)
        raise Invalid, "unknown key" unless key

        raw = B64.decode(parts[2])
        raise Invalid, "bad signature length" unless raw.bytesize == 64

        der = OpenSSL::ASN1::Sequence([
          OpenSSL::ASN1::Integer(OpenSSL::BN.new(raw[0, 32], 2)),
          OpenSSL::ASN1::Integer(OpenSSL::BN.new(raw[32, 32], 2)),
        ]).to_der
        raise Invalid, "signature does not verify" unless key.verify("SHA256", der, "#{parts[0]}.#{parts[1]}")

        claims = JSON.parse(B64.decode(parts[1]))
        raise Invalid, "the claims are not a JSON object" unless claims.is_a?(Hash)

        [header, claims]
      rescue JSON::ParserError, OpenSSL::PKey::PKeyError, ArgumentError, TypeError
        raise Invalid, "malformed token"
      end

      # not_before checks nbf (RFC 7519 §4.1.5) when a token carries one:
      # fog never sends it, but a token that says it is not yet valid isn't.
      def self.not_before!(claims, now, skew)
        return unless claims.key?("nbf")
        raise Invalid, "not yet valid" unless claims["nbf"].is_a?(Integer) && claims["nbf"] - skew <= now
      end
    end

    # State: what a flow must remember between leaving and coming back,
    # sealed (AES-256-GCM under a key derived from the client secret) and
    # carried in the state parameter itself — and bound to the browser that
    # started it by a random value in a cookie, so a callback URL pasted into
    # someone else's browser signs no one in (login CSRF).
    module State
      LIFETIME = 600

      # Each sealed thing has a purpose, bound in as associated data: a
      # sealed link (purpose "pending") can never be replayed as a state.
      PURPOSES = %w[state pending].freeze

      def self.key(config) = OpenSSL::HMAC.digest("SHA256", config.client_secret, "fog-signin state v1")

      def self.seal(config, payload, purpose: "state")
        raise ArgumentError, "unknown purpose #{purpose}" unless PURPOSES.include?(purpose)

        cipher = OpenSSL::Cipher.new("aes-256-gcm").encrypt
        cipher.key = key(config)
        iv = cipher.random_iv
        cipher.auth_data = "fog-signin #{purpose}"
        ct = cipher.update(JSON.generate(payload)) + cipher.final
        B64.encode(iv + cipher.auth_tag + ct)
      end

      def self.open(config, sealed, purpose: "state")
        raise ArgumentError, "unknown purpose #{purpose}" unless PURPOSES.include?(purpose)

        raw = B64.decode(sealed.to_s)
        raise Invalid, "bad state" if raw.bytesize < 29

        cipher = OpenSSL::Cipher.new("aes-256-gcm").decrypt
        cipher.key = key(config)
        cipher.iv = raw[0, 12]
        cipher.auth_tag = raw[12, 16]
        cipher.auth_data = "fog-signin #{purpose}"
        JSON.parse(cipher.update(raw[28..]) + cipher.final)
      rescue OpenSSL::Cipher::CipherError, JSON::ParserError
        raise Invalid, "bad state"
      end
    end

    # safe_return_to keeps a return address only when it is a path on this
    # host, and "/" otherwise. A browser drops tabs and newlines from a URL
    # and reads a backslash as a slash, so "/\t/evil.example" or "/\\evil"
    # would become another origin: any control character or backslash is
    # refused outright, and what is left must parse as a bare path.
    def self.safe_return_to(raw)
      s = raw.to_s
      return "/" unless s.start_with?("/") && !s.start_with?("//")
      return "/" if s.match?(/[\x00-\x1f\x7f\\]/) || !s.valid_encoding?

      u = URI.parse(s)
      u.scheme.nil? && u.host.nil? && u.userinfo.nil? && u.path.start_with?("/") && !u.path.start_with?("//") ? s : "/"
    rescue URI::InvalidURIError
      "/"
    end

    def self.secure_compare(a, b)
      a.is_a?(String) && b.is_a?(String) && a.bytesize == b.bytesize && OpenSSL.fixed_length_secure_compare(a, b)
    end

    # Flow: start and finish one sign-in.
    class Flow
      SKEW = 60
      SCOPE = "openid profile roles"

      # Start is what the app keeps (binding goes in a cookie) and where it
      # sends the browser.
      Start = Struct.new(:url, :binding, keyword_init: true)

      def initialize(provider, redirect_uri:, scope: SCOPE, clock: -> { Time.now }, http: Http.new)
        @provider, @redirect_uri, @scope, @clock, @http = provider, redirect_uri, scope, clock, http
      end

      def config = @provider.config

      # start pushes the request (RFC 9126) when the provider supports it,
      # and returns the URL to send the browser to.
      def start(return_to: "/", prompt: nil, max_age: nil, acr: nil, login_hint: nil)
        verifier = B64.encode(SecureRandom.random_bytes(32))
        nonce = B64.encode(SecureRandom.random_bytes(16))
        binding = B64.encode(SecureRandom.random_bytes(16))
        state = State.seal(config, {
          "v" => verifier, "n" => nonce, "r" => return_to,
          "b" => B64.encode(OpenSSL::Digest::SHA256.digest(binding)), "e" => @clock.call.to_i + State::LIFETIME,
        })
        params = {
          "response_type" => "code", "client_id" => config.client_id, "redirect_uri" => @redirect_uri,
          "scope" => @scope, "state" => state, "nonce" => nonce,
          "code_challenge" => B64.encode(OpenSSL::Digest::SHA256.digest(verifier)), "code_challenge_method" => "S256",
        }
        params["prompt"] = prompt if prompt
        params["max_age"] = max_age.to_s if max_age
        params["acr_values"] = acr if acr
        params["login_hint"] = login_hint if login_hint
        authorize = @provider.endpoint("authorization_endpoint")
        par = @provider.discovery["pushed_authorization_request_endpoint"]
        query = if par
                  pushed = @http.post_form(config.back_channel(par), params, basic: [config.client_id, config.client_secret])
                  { "client_id" => config.client_id, "request_uri" => pushed.fetch("request_uri") }
                else
                  params
                end
        Start.new(url: "#{authorize}?#{URI.encode_www_form(query)}", binding: binding)
      end

      # finish completes the callback: params is the callback's query, binding
      # the cookie start set. Returns [Identity, return_to].
      def finish(params, binding:)
        sealed = State.open(config, params["state"])
        raise Invalid, "sign-in took too long; start again" if sealed["e"].to_i < @clock.call.to_i
        raise Invalid, "this sign-in was started in another browser" unless
          binding && Signin.secure_compare(B64.encode(OpenSSL::Digest::SHA256.digest(binding)), sealed["b"])
        # RFC 9207: the response says who sent it; a mix-up attack does not.
        # A provider that promises iss must always send it (§2.4).
        iss = params["iss"]
        if iss.nil? && @provider.discovery["authorization_response_iss_parameter_supported"] == true
          raise Invalid, "the response did not say where it came from"
        end
        raise Invalid, "the response came from #{iss.inspect}" if iss && iss != config.issuer
        raise Denied.new(params["error"], params["error_description"]) if params["error"]

        code = params["code"].to_s
        raise Invalid, "no code" if code.empty?

        tokens = @http.post_form(config.back_channel(@provider.endpoint("token_endpoint")), {
          "grant_type" => "authorization_code", "code" => code, "redirect_uri" => @redirect_uri, "code_verifier" => sealed["v"],
        }, basic: [config.client_id, config.client_secret])
        [verify_id_token(tokens.fetch("id_token"), nonce: sealed["n"]), sealed["r"]]
      end

      # verify_id_token checks an ID token for this client. nonce is the one
      # the request sent; nil only for a grant that sends none (the device
      # grant), when the token must carry none either.
      def verify_id_token(raw, nonce:)
        header, c = JWT.decode(raw, @provider)
        raise Invalid, "not an ID token" unless header["typ"].nil? || header["typ"].to_s.casecmp?("JWT")
        now = @clock.call.to_i
        aud = c["aud"]
        raise Invalid, "issuer" unless c["iss"] == config.issuer
        raise Invalid, "audience" unless aud == config.client_id || aud == [config.client_id]
        raise Invalid, "authorized party" if c.key?("azp") && c["azp"] != config.client_id
        raise Invalid, "expired" unless c["exp"].is_a?(Integer) && c["exp"] + SKEW > now
        raise Invalid, "issued in the future" unless c["iat"].is_a?(Integer) && c["iat"] - SKEW <= now
        JWT.not_before!(c, now, SKEW)
        if nonce.nil?
          raise Invalid, "nonce" if c.key?("nonce")
        else
          raise Invalid, "nonce" if nonce.to_s.empty? || !Signin.secure_compare(c["nonce"].to_s, nonce.to_s)
        end
        raise Invalid, "no subject" if c["sub"].to_s.empty?

        Identity.new(issuer: c["iss"], subject: c["sub"], handle: c["preferred_username"], name: c["name"],
                     email: c["email"], email_verified: c["email_verified"] == true, roles: Array(c["roles"]),
                     acr: c["acr"], amr: Array(c["amr"]), sid: c["sid"],
                     auth_time: c["auth_time"].is_a?(Integer) ? Time.at(c["auth_time"]) : nil, home: c["fog_home"],
                     visitor: c["fog_visitor"] == true, id_token: raw, session_exp: Signin.session_exp(c, now))
      end

      # end_session_url signs the person out of the provider too, returning
      # them to return_to (which the provider must know).
      def end_session_url(id_token:, return_to:)
        endpoint = @provider.discovery["end_session_endpoint"]
        return nil if endpoint.to_s.empty?

        "#{endpoint}?#{URI.encode_www_form('id_token_hint' => id_token, 'post_logout_redirect_uri' => return_to)}"
      end
    end

    # Device: sign someone in on a screen with no keyboard worth typing on
    # (RFC 8628). The app server — always a confidential client — asks fog for
    # a code, the screen shows it, the person approves it on their phone at
    # verification_uri, and the app's poll becomes a verified Identity.
    class Device
      Code = Struct.new(:device_code, :user_code, :verification_uri, :verification_uri_complete, :expires_in, :interval,
                        keyword_init: true)

      # Pending is "ask again in interval seconds"; slow_down raises it.
      class Pending < Error
        attr_reader :interval

        def initialize(interval)
          @interval = interval
          super("authorization pending")
        end
      end

      def initialize(provider, scope: Flow::SCOPE, clock: -> { Time.now }, http: Http.new)
        @provider, @scope, @clock, @http = provider, scope, clock, http
      end

      def start(device_name: nil)
        cfg = @provider.config
        endpoint = @provider.endpoint("device_authorization_endpoint")
        form = { "scope" => @scope }
        form["device_name"] = device_name.to_s[0, 60] if device_name
        r = @http.post_form(cfg.back_channel(endpoint), form, basic: [cfg.client_id, cfg.client_secret])
        Code.new(**r.slice("device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval")
          .transform_keys(&:to_sym))
      end

      # poll returns the Identity once approved, raises Pending while
      # waiting (with the interval to wait), and Denied when the person said
      # no or the code expired.
      def poll(code)
        cfg = @provider.config
        tokens = @http.post_form(cfg.back_channel(@provider.endpoint("token_endpoint")), {
          "grant_type" => "urn:ietf:params:oauth:grant-type:device_code", "device_code" => code.device_code,
        }, basic: [cfg.client_id, cfg.client_secret])
        flow = Flow.new(@provider, redirect_uri: "", clock: @clock, http: @http)
        # A device sign-in carries no nonce: the code itself, bound to this
        # client, is what ties the answer to the request.
        flow.verify_id_token(tokens.fetch("id_token"), nonce: nil)
      rescue Denied => e
        case e.code
        when "authorization_pending" then raise Pending.new(code.interval.to_i)
        when "slow_down" then raise Pending.new(code.interval.to_i + 5)
        else raise
        end
      end
    end

    # Backchannel: OIDC Back-Channel Logout 1.0 §2.6 — what a valid logout
    # token must be before the app ends anyone's sessions.
    class Backchannel
      EVENT = "http://schemas.openid.net/event/backchannel-logout"
      # fog's own event: end every session started from a sign-in issued
      # before "before" (unix seconds). fog sends it after an emergency key
      # rotation, when a leaked key could have minted a sign-in for anyone.
      # The token's sid is ALL_SID, which no session carries.
      SESSIONS_REVOKED = "urn:fog:event:sessions-revoked"
      ALL_SID = "urn:fog:sid:all"
      # all_before is a Time when the token ends every session from before it.
      Logout = Struct.new(:subject, :sid, :jti, :all_before, keyword_init: true) do
        def all? = !all_before.nil?
      end

      def initialize(provider, clock: -> { Time.now })
        @provider, @clock = provider, clock
        @seen = {}
        @mutex = Mutex.new
      end

      def verify(raw)
        _, c = JWT.decode(raw, @provider, typ: "logout+jwt")
        cfg = @provider.config
        now = @clock.call.to_i
        aud = c["aud"]
        raise Invalid, "issuer" unless c["iss"] == cfg.issuer
        raise Invalid, "audience" unless aud == cfg.client_id || Array(aud).include?(cfg.client_id)
        raise Invalid, "stale" unless c["iat"].is_a?(Integer) && (now - c["iat"]).abs <= 300
        raise Invalid, "expired" if c.key?("exp") && !(c["exp"].is_a?(Integer) && c["exp"] + Flow::SKEW > now)
        JWT.not_before!(c, now, Flow::SKEW)
        raise Invalid, "not a logout token" unless c["events"].is_a?(Hash) && c["events"].key?(EVENT)
        raise Invalid, "a logout token never carries a nonce" if c.key?("nonce")
        raise Invalid, "no subject or session" if c["sub"].to_s.empty? && c["sid"].to_s.empty?
        raise Invalid, "no jti" if c["jti"].to_s.empty?

        @mutex.synchronize do
          @seen.delete_if { |_, at| now - at > 600 }
          raise Invalid, "replayed" if @seen.key?(c["jti"])

          @seen[c["jti"]] = now
        end
        all = c["events"][SESSIONS_REVOKED]
        if all
          raise Invalid, "a sessions-revoked token names every session" unless c["sid"] == ALL_SID && c["sub"].nil?
          raise Invalid, "sessions-revoked needs before" unless all.is_a?(Hash) && all["before"].is_a?(Integer)

          return Logout.new(sid: ALL_SID, jti: c["jti"], all_before: Time.at(all["before"]))
        end
        Logout.new(subject: c["sub"], sid: c["sid"], jti: c["jti"])
      end
    end

    # The codes a sign-in that refuses a visitor redirects with
    # (/login#signin_error=…), so a login page can say why.
    VISITOR_REFUSALS = {
      "visitors_not_hosted" => "This app doesn't let people from other households in.",
      "visitor_mismatch" => "This sign-in doesn't match the account it's linked to.",
      "not_set_up" => "This app isn't set up yet; someone in its household has to sign in first.",
    }.freeze

    # Linker: what a verified identity means for the app's accounts. Pure:
    # the app answers the questions, the linker decides.
    #
    #   :sign_in    an account is linked to this identity
    #   :bootstrap  the app has no accounts and this is its admin: the
    #               setup wizard, on fog
    #   :adopt      provisioning (SCIM) made an account for this subject
    #   :link       the app has accounts; the person must prove one is
    #               theirs, or start fresh — never matched by email
    #   :provision  a new account
    #   :refuse     the identity and the linked account disagree about
    #               whether this is a visitor — never sign one in as the other
    #   :visit      a visitor from a paired household with no
    #               account here yet: the app makes a visitor's account, or
    #               refuses — never bootstrap, adopt or link, which are for
    #               this household's people
    module Linker
      Decision = Struct.new(:action, :account, keyword_init: true)

      # adapter responds to:
      #   find_by_identity(issuer, subject) -> account | nil
      #   find_by_external_id(subject)      -> account | nil   (SCIM)
      #   account_count                     -> Integer
      #   visitor_account?(account)         -> Boolean          optional: apps
      #                                        that host visitors answer it
      def self.resolve(identity, adapter, admin_role:)
        if (a = adapter.find_by_identity(identity.issuer, identity.subject))
          if adapter.respond_to?(:visitor_account?) && adapter.visitor_account?(a) != identity.visitor?
            return Decision.new(action: :refuse, account: a)
          end
          return Decision.new(action: :sign_in, account: a)
        end
        return Decision.new(action: :visit) if identity.visitor?
        if (a = adapter.find_by_external_id(identity.subject))
          return Decision.new(action: :adopt, account: a)
        end
        return Decision.new(action: :bootstrap) if adapter.account_count.zero? && identity.role == admin_role
        return Decision.new(action: :link) if adapter.account_count.positive?

        Decision.new(action: :provision)
      end
    end

    # Proof: is this app provably running on fog? Only then does it turn
    # local sign-in off by itself. The fog that recognises this app's
    # own FOG_API_TOKEN must sign — with the issuer's own key — a statement
    # naming this issuer and this client.
    module Proof
      @mutex = Mutex.new
      @last_failure = nil

      # check_throttled is check for an unauthenticated page: a failed proof
      # is not asked again for `every` seconds, so a reloaded login page
      # cannot keep a server thread waiting on an unreachable fogd.
      def self.check_throttled(provider, env = ENV, every: 30, http: Http.new, clock: -> { Time.now })
        @mutex.synchronize do
          return false if @last_failure && clock.call - @last_failure < every
        end
        ok = check(provider, env, http: http)
        @mutex.synchronize { @last_failure = ok ? nil : clock.call }
        ok
      end

      def self.reset_throttle! = @mutex.synchronize { @last_failure = nil }

      def self.check(provider, env = ENV, http: Http.new)
        api, token = env["FOG_API_URL"], env["FOG_API_TOKEN"]
        return false if api.to_s.empty? || token.to_s.empty?

        body = http.get_json("#{api.chomp('/')}/app/v1/signin", bearer: token)
        verify(body.fetch("proof"), provider)
      rescue Error, KeyError, SocketError, SystemCallError, Timeout::Error, IOError
        false
      end

      # verify is the proof itself: signed by the issuer's key as a proof (its
      # typ), naming this issuer and this client, and recent.
      def self.verify(raw, provider, clock: -> { Time.now })
        _, c = JWT.decode(raw, provider, typ: "fog-signin-proof+jwt")
        JWT.not_before!(c, clock.call.to_i, 60)
        c["issuer"] == provider.config.issuer && c["client_id"] == provider.config.client_id &&
          c["iat"].is_a?(Integer) && (clock.call.to_i - c["iat"]).abs < 300
      rescue Error
        false
      end
    end

    # Http: just enough Net::HTTP, bounded like every other call this gem
    # makes (FogClient.connection).
    class Http
      def get_json(url, bearer: nil)
        req = Net::HTTP::Get.new(URI(url))
        req["Authorization"] = "Bearer #{bearer}" if bearer
        call(req)
      end

      def post_form(url, form, basic: nil)
        req = Net::HTTP::Post.new(URI(url))
        req.set_form_data(form)
        # RFC 6749 §2.3.1: form-encode each half before Basic encoding.
        req.basic_auth(URI.encode_www_form_component(basic[0]), URI.encode_www_form_component(basic[1])) if basic
        call(req)
      end

      private

      def call(req)
        uri = req.uri
        res = FogClient.connection(uri).request(req)
        body = res.body.to_s.empty? ? {} : JSON.parse(res.body)
        return body if res.is_a?(Net::HTTPSuccess)

        raise Denied.new(body["error"] || "http_#{res.code}", body["error_description"]) if body.is_a?(Hash)

        raise Error, "#{uri.path}: #{res.code}"
      rescue JSON::ParserError
        raise Error, "#{uri.path}: not JSON"
      end
    end
  end
end
