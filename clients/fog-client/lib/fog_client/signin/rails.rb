# frozen_string_literal: true

# FogClient::Signin::Rails — the controller half every Rails app in the fleet
# shares (fog:docs/signin-contract.md). Include it into a controller that
# skips the app's own authentication, route its four actions, and supply an
# adapter; the app keeps its own users, sessions and roles.
#
#   class Auth::OidcController < ActionController::API
#     include FogClient::Signin::Rails
#     private
#     def signin_adapter = SigninAdapter.new
#   end
#
#   get  "auth/oidc/start"        => "auth/oidc#start"
#   get  "auth/oidc/callback"     => "auth/oidc#callback"
#   post "auth/oidc/backchannel"  => "auth/oidc#backchannel"
#   post "auth/oidc/link"         => "auth/oidc#link"     (JSON, from the SPA)
#
# The adapter answers the app's questions and does the app's writes:
#
#   admin_role                               -> "admin"
#   find_by_identity(issuer, subject)        -> account | nil
#   find_by_external_id(subject)             -> account | nil      (SCIM)
#   account_count                            -> Integer
#   bootstrap!(identity)                     -> account   the first admin
#   provision!(identity)                     -> account   a new account
#   link!(account, identity)                 -> void      remember the pair
#   authenticate_local(login, password)      -> account | nil      (link page)
#   account_from_request(request)            -> account | nil      optional: the
#                                               app's own signed-in session, for
#                                               apps without passwords
#   apply_role!(account, role)               -> void
#   visit!(identity)                         -> account   optional: a visitor's
#                                               account; without it the
#                                               app hosts no visitors. Raise
#                                               Denied("not_set_up") when the
#                                               app has no household yet. link!
#                                               follows, to remember the visitor
#   visitor_account?(account)                -> Boolean   with visit!: whether an
#                                               account is a visitor's, so a
#                                               visitor never signs in as anyone
#                                               else, nor anyone else as one
#   start_session!(account, identity, expires_at:) -> String  a one-time handoff
#                                               code. The session it starts
#                                               must end by expires_at
#                                               (identity.expires_at): fog's
#                                               own session's end, capped at
#                                               30 days (12 hours for a
#                                               visitor). An adapter whose
#                                               start_session! takes no
#                                               expires_at must read
#                                               identity.expires_at itself
#   end_sessions!(sid:, subject:, issuer:)   -> Integer   how many ended; issuer
#                                               is passed when the method takes it,
#                                               so logout-by-subject touches only
#                                               this provider's identities
#   end_all_sessions!(before:, issuer:)      -> Integer   end every session started
#                                               from a sign-in before `before` (a
#                                               Time): fog's emergency key rotation.
#                                               Without it the kit answers 501, so
#                                               fog keeps trying and its Inbox says
#                                               the app hasn't heard
#
# The controller may define signin_required_acr (e.g.
# "urn:fog:acr:phr" for an app that holds money): it is requested at start
# and checked at the callback, refusing a sign-in that did not meet it.
#
# What else every adopter does (the apps learned these the hard way):
#   - Once fog-only, refuse EVERY local way in: password and passkey
#     sign-in, invite acceptance, sign-up, the setup wizard and its restore.
#   - Throttle the fog-only proof (FogClient::Signin::Proof.check_throttled)
#     when it runs from an unauthenticated page.
#   - Filter code, pending and handoff from request logs.
#   - Give accounts fog creates an email only when fog marks it verified and
#     nobody holds it; otherwise a unique placeholder under fog.invalid. Never
#     use an email to find anyone.
#   - Prune sessions whose handoff was never redeemed.
#   - Don't rate-limit link yourself: the kit does (as "fog-link"), and a
#     second limit on the same action halves it.
#   - End every session by identity.expires_at, which start_session!
#     receives: an app session never outlives the fog session it came from,
#     the end the ID token states (fog_session_exp). fog also sends a
#     back-channel logout then; the cap holds even when that is lost.
#   - Hosting visitors: never let a visitor mint a credential that outlives
#     the visit (API tokens, passkeys, device pairings); their expires_at is
#     the visit's end, at most 12 hours; never count them as household
#     accounts (account_count) or let them be admin.
# Nothing here matches accounts by email: an existing account is linked only
# by its owner signing in to it once (Linker, :link).

require "json"
require "securerandom"

module FogClient
  module Signin
    module Rails
      COOKIE = "__Host-fog_signin"
      # LINK_COOKIE binds a pending link to the browser fog sent back: a
      # /auth/link URL handed to someone else links nothing.
      LINK_COOKIE = "__Host-fog_link"
      PENDING_TTL = 600
      PHR = "urn:fog:acr:phr"

      # The link step takes passwords: limit it like the app's own sign-in.
      def self.included(base)
        return unless base.respond_to?(:rate_limit)

        base.rate_limit(to: 10, within: 3.minutes, only: :link, name: "fog-link")
      end

      # GET /auth/oidc/start?return_to=/path&prompt=login
      def start
        flow = signin_flow
        return head(:not_found) unless flow

        started = flow.start(return_to: safe_return_to(params[:return_to]), prompt: params[:prompt].presence,
                             max_age: params[:max_age].presence&.to_i, login_hint: params[:login_hint].presence,
                             acr: required_acr)
        set_binding_cookie(started.binding)
        redirect_to started.url, allow_other_host: true
      rescue FogClient::Signin::Error => e
        signin_failed(e)
      end

      # GET /auth/oidc/callback — fog sent the browser back.
      def callback
        flow = signin_flow
        return head(:not_found) unless flow

        # One value per parameter: a repeated state, code or iss is not a
        # response fog sent.
        repeated = URI.decode_www_form(request.query_string.to_s).map(&:first).tally.select { |_, n| n > 1 }.keys
        raise FogClient::Signin::Invalid, "repeated #{repeated.join(', ')}" if repeated.any?

        identity, return_to = flow.finish(request.query_parameters, binding: request.cookies[COOKIE])
        clear_binding_cookie
        if required_acr == PHR && identity.acr != PHR
          raise FogClient::Signin::Denied.new("insufficient_assurance", "this app asks for a passkey")
        end
        adapter = signin_adapter
        decision = FogClient::Signin::Linker.resolve(identity, adapter, admin_role: adapter.admin_role)
        account = case decision.action
                  when :sign_in, :adopt then decision.account
                  when :refuse
                    raise FogClient::Signin::Denied.new("visitor_mismatch", "visitor and account disagree")
                  when :bootstrap then adapter.bootstrap!(identity)
                  when :provision then adapter.provision!(identity)
                  when :visit
                    unless adapter.respond_to?(:visit!)
                      raise FogClient::Signin::Denied.new("visitors_not_hosted", "this app hosts no visitors")
                    end

                    adapter.visit!(identity)
                  when :link
                    link_binding = FogClient::Signin::B64.encode(SecureRandom.random_bytes(16))
                    set_link_cookie(link_binding)
                    pending = seal_pending(identity, return_to, link_binding)
                    return redirect_to("/auth/link##{URI.encode_www_form('pending' => pending)}")
                  end
        adapter.link!(account, identity) unless decision.action == :sign_in
        finish_sign_in(adapter, account, identity, return_to)
      rescue FogClient::Signin::Denied => e
        redirect_to "/login##{URI.encode_www_form('signin_error' => e.code)}"
      rescue FogClient::Signin::Error => e
        signin_failed(e)
      rescue StandardError => e
        # The app's own failure (an adapter raising, a validation): report
        # it and send the person back to sign in, never a 500 page.
        signin_failed(e, report: true)
      end

      # POST /auth/oidc/link {pending, login, password} — the person proves an
      # existing account is theirs; or {pending, fresh: true} starts anew. An
      # app whose accounts have no passwords (passkeys only) lets the person
      # sign in to it the usual way first and sends that session along: the
      # adapter's account_from_request is the proof then.
      def link
        flow = signin_flow
        return head(:not_found) unless flow

        pending = open_pending(params.require(:pending).to_s)
        unless FogClient::Signin.secure_compare(digest(request.cookies[LINK_COOKIE].to_s), pending["b"].to_s)
          raise FogClient::Signin::Invalid, "this link was started in another browser"
        end
        identity = FogClient::Signin::Identity.new(**pending["identity"].transform_keys(&:to_sym))
        # Visitors never reach :link; a sealed one is not a link to honour.
        raise FogClient::Signin::Denied.new("visitor_mismatch", "a visitor has no account here to link") if identity.visitor?
        adapter = signin_adapter
        if adapter.find_by_identity(identity.issuer, identity.subject)
          return render(json: { errors: ["This sign-in is already connected; sign in again."] }, status: :conflict)
        end

        account = if params[:fresh].to_s == "true"
                    adapter.provision!(identity)
                  elsif adapter.respond_to?(:account_from_request) && (signed_in = adapter.account_from_request(request))
                    signed_in
                  elsif params[:login].present? && params[:password].present?
                    adapter.authenticate_local(params[:login].to_s, params[:password].to_s)
                  end
        return render(json: { errors: ["That didn't prove the account is yours."] }, status: :unauthorized) unless account

        adapter.link!(account, identity)
        adapter.apply_role!(account, identity.role) if identity.role
        code = start_session(adapter, account, identity)
        clear_link_cookie
        render json: { handoff: code, return_to: pending["return_to"] }
      rescue FogClient::Signin::Denied => e
        render json: { errors: [e.message] }, status: :forbidden
      rescue FogClient::Signin::Error
        render json: { errors: ["This sign-in expired; start again."] }, status: :unprocessable_content
      rescue StandardError => e
        report(e)
        render json: { errors: ["That didn't work; sign in again."] }, status: :unprocessable_content
      end

      # POST /auth/oidc/backchannel (form: logout_token) — fog says a session
      # or a person is gone (OIDC Back-Channel Logout 1.0).
      def backchannel
        flow = signin_flow
        return head(:not_found) unless flow

        out = signin_backchannel.verify(params.require(:logout_token).to_s)
        adapter = signin_adapter
        response.headers["Cache-Control"] = "no-store"
        if out.all?
          # Every session from before the rotation, whoever it names: a
          # leaked key could have signed anyone in.
          return head(:not_implemented) unless adapter.respond_to?(:end_all_sessions!)

          adapter.end_all_sessions!(before: out.all_before, issuer: signin_config.issuer)
          return head(:ok)
        end
        subject = out.sid.to_s.empty? ? out.subject : nil
        takes_issuer = adapter.method(:end_sessions!).parameters.any? { |_, name| name == :issuer }
        if takes_issuer
          adapter.end_sessions!(sid: out.sid, subject: subject, issuer: signin_config.issuer)
        else
          adapter.end_sessions!(sid: out.sid, subject: subject)
        end
        response.headers["Cache-Control"] = "no-store"
        head :ok
      rescue FogClient::Signin::Error, ActionController::ParameterMissing
        response.headers["Cache-Control"] = "no-store"
        render json: { error: "invalid_request" }, status: :bad_request
      end

      private

      def finish_sign_in(adapter, account, identity, return_to)
        adapter.apply_role!(account, identity.role) if identity.role
        code = start_session(adapter, account, identity)
        redirect_to "/auth/complete##{URI.encode_www_form('handoff' => code, 'return_to' => return_to)}"
      end

      def signin_config = FogClient::Signin.config

      # start_session asks the adapter for a session that ends by the
      # identity's expires_at, handed over as a keyword when the adapter's
      # start_session! takes it.
      def start_session(adapter, account, identity)
        takes = adapter.method(:start_session!).parameters.any? { |kind, name| name == :expires_at || kind == :keyrest }
        takes ? adapter.start_session!(account, identity, expires_at: identity.expires_at) : adapter.start_session!(account, identity)
      end

      def signin_provider
        cfg = signin_config
        return nil unless cfg

        # One provider per process: discovery and keys are cached on it.
        FogClient::Signin::Rails.provider_for(cfg)
      end

      def signin_flow
        provider = signin_provider
        provider && FogClient::Signin::Flow.new(provider, redirect_uri: "#{request.base_url}/auth/oidc/callback")
      end

      def signin_backchannel = FogClient::Signin::Rails.backchannel_for(signin_provider)

      def self.provider_for(cfg)
        @providers ||= {}
        @providers[[cfg.issuer, cfg.client_id]] ||= FogClient::Signin::Provider.new(cfg)
      end

      def self.backchannel_for(provider)
        @backchannels ||= {}
        @backchannels[provider.object_id] ||= FogClient::Signin::Backchannel.new(provider)
      end

      # A return path is a path on this host, nothing else
      # (FogClient::Signin.safe_return_to).
      def safe_return_to(raw) = FogClient::Signin.safe_return_to(raw)

      # Set on the response directly: API-only apps run without the cookie
      # middleware, and this is the one cookie they need. Reading it back is
      # plain Rack (request.cookies).
      def set_binding_cookie(value)
        response.set_cookie(COOKIE, { value: value, httponly: true, secure: true, same_site: :lax, path: "/",
                                      max_age: FogClient::Signin::State::LIFETIME })
      end

      def clear_binding_cookie = response.delete_cookie(COOKIE, { path: "/", secure: true })

      def set_link_cookie(value)
        response.set_cookie(LINK_COOKIE, { value: value, httponly: true, secure: true, same_site: :strict, path: "/",
                                           max_age: PENDING_TTL })
      end

      def clear_link_cookie = response.delete_cookie(LINK_COOKIE, { path: "/", secure: true })

      def digest(value) = FogClient::Signin::B64.encode(OpenSSL::Digest::SHA256.digest(value.to_s))

      def seal_pending(identity, return_to, link_binding)
        FogClient::Signin::State.seal(signin_config, {
          "identity" => identity.to_h.except(:id_token, :auth_time).transform_keys(&:to_s),
          "return_to" => return_to, "e" => Time.now.to_i + PENDING_TTL, "b" => digest(link_binding),
        }, purpose: "pending")
      end

      def open_pending(sealed)
        p = FogClient::Signin::State.open(signin_config, sealed, purpose: "pending")
        raise FogClient::Signin::Invalid, "expired" if p["e"].to_i < Time.now.to_i

        p
      end

      def required_acr = respond_to?(:signin_required_acr, true) ? signin_required_acr : nil

      def signin_failed(error, report: false)
        ::Rails.logger.warn("fog sign-in failed: #{error.class}: #{error.message}")
        report(error) if report
        redirect_to "/login##{URI.encode_www_form('signin_error' => 'failed')}"
      end

      def report(error)
        ::Rails.error.report(error, handled: true, source: "fog-signin") if ::Rails.respond_to?(:error)
      end
    end
  end
end
