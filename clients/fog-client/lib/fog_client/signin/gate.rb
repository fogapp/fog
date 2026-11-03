# frozen_string_literal: true

# FogClient::Signin::Gate — for an app behind fog's gate (a route with
# auth: true; fog:docs/signin-contract.md "The gate"): who is at the door,
# from the signed Fog-Assertion fog adds to every request it lets through,
# never from the bare Remote-* headers beside it.
#
# Caddy scrubs client-sent Remote-* and Fog-Assertion on the way in, but any
# container that can reach the app directly can send whatever headers it
# likes. A Fog-Assertion is an ES256 JWT fog signed for one of this app's
# own hosts, valid for a minute: verify it and the forgery is gone.
#
# By default the middleware verifies the assertion (signature against fog's
# keys, issuer, audience = this app's own gated host as fog reports it, and
# expiry), puts the identity in env["fog.gate.identity"], deletes every
# Remote-* header the request brought, and writes back only what the
# assertion says — so an app that reads Remote-User keeps working, on
# verified values. No valid assertion, no identity and no Remote-* at all.
#
#   require "fog_client/signin/gate"
#   # config/application.rb (any Rack app: `use` it)
#   config.middleware.insert_before 0, FogClient::Signin::Gate::Middleware
#
#   class ApplicationController < ActionController::Base
#     include FogClient::Signin::Gate::Rails
#     before_action :require_fog_gate!   # 401 without a verified person
#     # fog_gate_identity.handle, .name, .roles, .subject …
#   end
#
# It needs FOG_API_URL and FOG_API_TOKEN, which fog gives every app: it asks
# fog (GET /app/v1/gate) for the issuer and the app's own audiences, and
# reads fog's keys through FOG_API_URL, so nothing rests on DNS or on a Host
# header a forger chooses.
#
# The opt-out, trust_remote_headers: true (or FOG_GATE_TRUST_REMOTE_HEADERS=1),
# keeps the Remote-* headers as they came: only for an app that cannot reach
# fog's API. It warns, because on a network an attacker can reach, those
# headers prove nothing.

require_relative "../../fog_client"

module FogClient
  module Signin
    module Gate
      ASSERTION_HEADER = "HTTP_FOG_ASSERTION"
      # Every header a client could send that an app might read as the
      # gate's: Remote-*, Fog-*, in any spelling a Rack server folds into the
      # same name (Remote-User, Remote_User, REMOTE.USER all land on
      # HTTP_REMOTE_USER or a neighbour). The middleware deletes them all and
      # writes back only what the assertion says.
      FORGEABLE = /\AHTTP_(REMOTE|FOG)[^A-Z0-9]?/i
      REMOTE_HEADERS = %w[HTTP_REMOTE_USER HTTP_REMOTE_NAME HTTP_REMOTE_GROUPS HTTP_REMOTE_EMAIL].freeze
      ENV_KEY = "fog.gate.identity"
      # An assertion lives 60 seconds; allow this much clock skew around it.
      SKEW = 5
      INFO_TTL = 300

      # Identity is who passed the gate, as the assertion says.
      Identity = Struct.new(:subject, :handle, :name, :roles, :sid, :amr, :acr, :home, :audience, :expires_at,
                            keyword_init: true) do
        def role = Array(roles).first
      end

      # Verifier checks assertions for one app.
      class Verifier
        def initialize(api_url:, api_token:, http: Http.new, clock: -> { Time.now })
          @api, @token, @http, @clock = api_url.chomp("/"), api_token, http, clock
          @mutex = Mutex.new
          @info = nil
          @info_at = nil
          @provider = nil
        end

        # info is fog's word on what this app accepts: {issuer, audiences}.
        def info
          @mutex.synchronize do
            if @info.nil? || @clock.call - @info_at > INFO_TTL
              got = @http.get_json("#{@api}/app/v1/gate", bearer: @token)
              raise Invalid, "fog named no issuer" if got["issuer"].to_s.empty?

              @info, @info_at = got, @clock.call
              @provider = nil if @provider && @provider.config.issuer != got["issuer"]
            end
            @info
          end
        end

        # provider is the key source JWT.decode asks: fog's published keys,
        # read through FOG_API_URL at the path /app/v1/gate names. Not OIDC
        # discovery — its issuer is fog's sign-in issuer, always https, while
        # the gate's names the setup's own scheme.
        def provider
          path = info["jwksPath"].to_s
          path = "/oauth2/jwks" if path.empty? || !path.start_with?("/")
          @mutex.synchronize { @provider ||= KeySet.new("#{@api}#{path}", http: @http, clock: @clock) }
        end

        # verify returns the Identity a Fog-Assertion vouches for, or raises
        # Invalid: signed by fog's current keys, issued by fog, for one of
        # this app's own hosts, and fresh.
        def verify(raw)
          _, c = JWT.decode(raw.to_s, provider, typ: "jwt")
          now = @clock.call.to_i
          audiences = Array(info["audiences"])
          raise Invalid, "issuer" unless c["iss"] == info["issuer"]
          raise Invalid, "not for this app" unless c["aud"].is_a?(String) && audiences.include?(c["aud"])
          raise Invalid, "expired" unless c["exp"].is_a?(Integer) && c["exp"] + SKEW > now
          raise Invalid, "issued in the future" unless c["iat"].is_a?(Integer) && c["iat"] - SKEW <= now
          JWT.not_before!(c, now, SKEW)
          raise Invalid, "no subject" if c["sub"].to_s.empty? || c["preferred_username"].to_s.empty?

          Identity.new(subject: c["sub"], handle: c["preferred_username"], name: c["name"], roles: Array(c["roles"]),
                       sid: c["sid"], amr: Array(c["amr"]), acr: c["acr"], home: c["fog_home"], audience: c["aud"],
                       expires_at: Time.at(c["exp"]))
        end
      end

      # KeySet is fog's published keys at one URL, refetched when an
      # assertion names a key it hasn't seen (at most every 30 seconds) and
      # every KEYS_TTL regardless, so a retired key stops verifying.
      class KeySet
        def initialize(url, http:, clock:)
          @url, @http, @clock = url, http, clock
          @mutex = Mutex.new
          @keys = {}
          @at = nil
        end

        def key(kid)
          @mutex.synchronize do
            fresh = @at && @clock.call - @at < Provider::KEYS_TTL
            return @keys[kid] if fresh && @keys.key?(kid)
            return nil if @at && @clock.call - @at < Provider::REFETCH_EVERY
          end
          set = @http.get_json(@url)
          @mutex.synchronize do
            @keys = Array(set["keys"]).each_with_object({}) do |jwk, h|
              next unless jwk["kty"] == "EC" && jwk["crv"] == "P-256" && jwk["kid"]
              next if jwk["use"] && jwk["use"] != "sig"

              h[jwk["kid"]] = JWT.ec_key(jwk)
            end
            @at = @clock.call
            @keys[kid]
          end
        end
      end

      # verifier is the app's Verifier, or nil when fog gave it no API.
      def self.verifier(env = ENV)
        api, token = env["FOG_API_URL"], env["FOG_API_TOKEN"]
        return nil if api.to_s.empty? || token.to_s.empty?

        @verifiers ||= {}
        @verifiers[[api, token]] ||= Verifier.new(api_url: api, api_token: token)
      end

      # identity verifies a Rack env's assertion: an Identity, or nil.
      def self.identity(env, verifier: self.verifier, logger: nil)
        raw = env[ASSERTION_HEADER].to_s
        return nil if raw.empty? || verifier.nil?

        verifier.verify(raw)
      rescue Error, KeyError, SocketError, SystemCallError, Timeout::Error, IOError => e
        logger&.warn("fog gate: assertion refused: #{e.class}: #{e.message}")
        nil
      end

      # StderrLog is the default logger: Logger is no longer a default gem.
      module StderrLog
        def self.warn(msg) = Kernel.warn(msg)
      end

      # Middleware is the gate's Rack half (see the header).
      class Middleware
        def initialize(app, verifier: :default, trust_remote_headers: nil, logger: nil, env: ENV)
          @app = app
          @verifier = verifier == :default ? Gate.verifier(env) : verifier
          @trust = trust_remote_headers.nil? ? env["FOG_GATE_TRUST_REMOTE_HEADERS"] == "1" : trust_remote_headers
          @logger = logger || StderrLog
          if @trust
            @logger.warn("fog gate: trusting Remote-* headers without verifying Fog-Assertion; any container that can " \
                         "reach this app can claim to be anyone")
          elsif @verifier.nil?
            @logger.warn("fog gate: no FOG_API_URL/FOG_API_TOKEN, so no assertion can be verified; every request is anonymous")
          end
        end

        def call(env)
          identity = Gate.identity(env, verifier: @verifier, logger: @logger)
          env[ENV_KEY] = identity
          unless @trust
            env.keys.grep(FORGEABLE).each { |h| env.delete(h) }
            if identity
              env["HTTP_REMOTE_USER"] = identity.handle.to_s
              env["HTTP_REMOTE_NAME"] = identity.name.to_s
              env["HTTP_REMOTE_GROUPS"] = identity.roles.join(",")
            end
          end
          @app.call(env)
        end
      end

      # Rails is a controller concern: fog_gate_identity is the verified
      # person (from the middleware, or verified here when it isn't
      # installed), and require_fog_gate! answers 401 without one.
      module Rails
        def fog_gate_identity
          return request.env[ENV_KEY] if request.env.key?(ENV_KEY)

          request.env[ENV_KEY] = Gate.identity(request.env, logger: (::Rails.logger if defined?(::Rails)))
        end

        def require_fog_gate!
          head(:unauthorized) unless fog_gate_identity
        end
      end
    end
  end
end
