# typed: true

# The typed surface of fog-client's sign-in kit (lib/fog_client/signin.rb,
# signin/rails.rb, signin/scim.rb), shipped with the gem so apps stop
# hand-copying a shim. Apps vendor the gem under vendor/, which their
# sorbet/config ignores: copy this file to sorbet/rbi/shims/fog_client_signin.rbi
# when syncing the gem (it is the canonical version of that shim).
module FogClient
  module Signin
    class Error < StandardError; end
    class Invalid < Error; end

    class Denied < Error
      sig { params(code: String, description: T.nilable(String)).void }
      def initialize(code, description = nil); end

      sig { returns(String) }
      def code; end
    end

    class Config
      sig { returns(String) }
      def issuer; end

      sig { returns(String) }
      def client_id; end
    end

    sig { params(env: T.untyped).returns(T.nilable(FogClient::Signin::Config)) }
    def self.config(env = ENV); end

    sig { params(env: T.untyped).returns(T::Boolean) }
    def self.enabled?(env = ENV); end

    class Identity
      sig { returns(String) }
      def issuer; end

      sig { returns(String) }
      def subject; end

      sig { returns(T.nilable(String)) }
      def handle; end

      sig { returns(T.nilable(String)) }
      def name; end

      sig { returns(T.nilable(String)) }
      def email; end

      sig { returns(T.nilable(T::Boolean)) }
      def email_verified; end

      sig { returns(T.nilable(String)) }
      def sid; end

      sig { returns(T.nilable(String)) }
      def role; end

      sig { returns(T::Array[String]) }
      def roles; end

      sig { returns(T.nilable(String)) }
      def home; end

      # A paired household's person, signed in by their own fog (§10.4).
      sig { returns(T::Boolean) }
      def visitor?; end

      # When the app's session for this sign-in must end (unix seconds):
      # fog's own session's end, capped at 30 days (12 hours for a visitor).
      sig { returns(T.nilable(Integer)) }
      def session_exp; end

      sig { returns(T.nilable(Time)) }
      def expires_at; end
    end

    class Provider
      sig { returns(FogClient::Signin::Config) }
      def config; end
    end

    module Proof
      sig { params(provider: FogClient::Signin::Provider, env: T.untyped, http: T.untyped).returns(T::Boolean) }
      def self.check(provider, env = ENV, http: nil); end

      sig do
        params(provider: FogClient::Signin::Provider, env: T.untyped, every: Integer, http: T.untyped, clock: T.untyped)
          .returns(T::Boolean)
      end
      def self.check_throttled(provider, env = ENV, every: 30, http: nil, clock: nil); end

      sig { void }
      def self.reset_throttle!; end

      sig { params(raw: String, provider: FogClient::Signin::Provider, clock: T.untyped).returns(T::Boolean) }
      def self.verify(raw, provider, clock: nil); end
    end

    # Included into Auth::OidcController; the actions are the kit's.
    module Rails
      sig { params(cfg: FogClient::Signin::Config).returns(FogClient::Signin::Provider) }
      def self.provider_for(cfg); end
    end

    module SCIM
      USER_SCHEMA = T.let(T.unsafe(nil), String)

      class BadRequest < FogClient::Signin::Error; end

      # What fog says about a person: externalId is the stable key.
      class User
        sig { returns(String) }
        def external_id; end

        sig { returns(String) }
        def user_name; end

        sig { returns(String) }
        def display_name; end

        sig { returns(T::Boolean) }
        def active; end

        sig { returns(T.nilable(String)) }
        def email; end

        sig { returns(T.nilable(String)) }
        def role; end
      end

      # Included into ScimController; the actions are the kit's.
      module Controller; end
    end

    # Behind the gate (signin/gate.rb): identity from a verified Fog-Assertion.
    module Gate
      class Identity
        sig { returns(String) }
        def subject; end

        sig { returns(String) }
        def handle; end

        sig { returns(T.nilable(String)) }
        def name; end

        sig { returns(T::Array[String]) }
        def roles; end

        sig { returns(T.nilable(String)) }
        def role; end
      end

      module Rails
        sig { returns(T.nilable(FogClient::Signin::Gate::Identity)) }
        def fog_gate_identity; end

        sig { void }
        def require_fog_gate!; end
      end
    end
  end
end
