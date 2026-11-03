# frozen_string_literal: true

# FogClient::Signin::SCIM — the app's side of fog's provisioning
# (fog:docs/signin-contract.md): the subset of SCIM 2.0 (RFC 7643/7644)
# fog speaks, so a person can exist in an app — be @-mentioned, be shared
# with — before they first sign in, and be switched off everywhere when
# fog suspends them. Still plain SCIM: Authentik or any provisioning client
# configured with SCIM_TOKEN works the same way.
#
# Rails apps include FogClient::Signin::SCIM::Controller into an
# ActionController::API controller outside their own authentication:
#
#   get   "scim/v2/Users"     => "scim#index"
#   post  "scim/v2/Users"     => "scim#create"
#   get   "scim/v2/Users/:id" => "scim#show"
#   put   "scim/v2/Users/:id" => "scim#replace"
#   patch "scim/v2/Users/:id" => "scim#patch"
#
# and supply scim_adapter:
#
#   scim_find(id)                     -> account | nil
#   scim_find_by_external_id(ext)     -> account | nil
#   scim_create!(user)                -> account     user is a SCIM::User
#   scim_replace!(account, user)      -> account     includes active: false
#   scim_resource(account)            -> Hash        the account as a SCIM User
#
# fog never deletes: a person no longer allowed arrives as active: false,
# and what they made stays the app's.

require "json"
require "openssl"

module FogClient
  module Signin
    module SCIM
      USER_SCHEMA = "urn:ietf:params:scim:schemas:core:2.0:User"
      LIST_SCHEMA = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
      ERROR_SCHEMA = "urn:ietf:params:scim:api:messages:2.0:Error"
      PATCH_SCHEMA = "urn:ietf:params:scim:api:messages:2.0:PatchOp"

      class BadRequest < FogClient::Signin::Error; end

      # User is what fog says about a person: externalId is the person
      # address, the stable key; userName and displayName may change.
      User = Struct.new(:external_id, :user_name, :display_name, :active, :email, :role, keyword_init: true)

      def self.parse_user(body)
        raise BadRequest, "body must be a SCIM User" unless body.is_a?(Hash) && Array(body["schemas"]).include?(USER_SCHEMA)

        ext = body["externalId"].to_s
        name = body["userName"].to_s
        raise BadRequest, "externalId is required" if ext.empty?
        raise BadRequest, "userName is required" if name.empty?

        email = Array(body["emails"]).find { |e| e.is_a?(Hash) && e["primary"] } || Array(body["emails"]).first
        role = Array(body["roles"]).find { |r| r.is_a?(Hash) && r["primary"] } || Array(body["roles"]).first
        display = body["displayName"].to_s
        User.new(external_id: ext, user_name: name, display_name: display.empty? ? name : display,
                 active: body.fetch("active", true) == true, email: email.is_a?(Hash) ? email["value"].to_s : nil,
                 role: role.is_a?(Hash) ? role["value"].to_s : nil)
      end

      # parse_filter accepts the one filter fog sends: externalId eq "…".
      def self.parse_filter(filter)
        return nil if filter.to_s.strip.empty?

        m = filter.to_s.strip.match(/\AexternalId eq "((?:[^"\\]|\\.)*)"\z/)
        raise BadRequest, "only externalId eq \"…\" is supported" unless m

        m[1].gsub(/\\(.)/, '\1')
      end

      # active_from_patch reads the one patch fog may send: replace active.
      def self.active_from_patch(body)
        raise BadRequest, "body must be a PatchOp" unless body.is_a?(Hash) && Array(body["schemas"]).include?(PATCH_SCHEMA)

        ops = Array(body["Operations"])
        raise BadRequest, "one operation, replacing active" unless ops.length == 1 && ops[0].is_a?(Hash)

        op = ops[0]
        value = op["path"].to_s == "active" ? op["value"] : (op["value"].is_a?(Hash) ? op["value"]["active"] : nil)
        raise BadRequest, "only active may be patched" unless op["op"].to_s.casecmp("replace").zero? && [true, false].include?(value)

        value
      end

      def self.authorized?(header, env = ENV)
        expected = env["SCIM_TOKEN"].to_s
        given = header.to_s.delete_prefix("Bearer ")
        !expected.empty? && header.to_s.start_with?("Bearer ") && FogClient::Signin.secure_compare(given, expected)
      end

      def self.list(resources)
        { "schemas" => [LIST_SCHEMA], "totalResults" => resources.length, "startIndex" => 1,
          "itemsPerPage" => resources.length, "Resources" => resources }
      end

      def self.error(status, detail)
        { "schemas" => [ERROR_SCHEMA], "status" => status.to_s, "detail" => detail }
      end

      module Controller
        def index
          return unless scim_authorized!

          ext = FogClient::Signin::SCIM.parse_filter(params[:filter])
          found = ext ? [scim_adapter.scim_find_by_external_id(ext)].compact : []
          scim_render(200, FogClient::Signin::SCIM.list(found.map { |a| scim_adapter.scim_resource(a) }))
        rescue FogClient::Signin::SCIM::BadRequest => e
          scim_render(400, FogClient::Signin::SCIM.error(400, e.message))
        end

        def show
          return unless scim_authorized!

          account = scim_adapter.scim_find(params[:id].to_s)
          return scim_render(404, FogClient::Signin::SCIM.error(404, "no such user")) unless account

          scim_render(200, scim_adapter.scim_resource(account))
        end

        def create
          return unless scim_authorized!

          user = FogClient::Signin::SCIM.parse_user(scim_body)
          if scim_adapter.scim_find_by_external_id(user.external_id)
            return scim_render(409, FogClient::Signin::SCIM.error(409, "a user with that externalId exists"))
          end

          scim_render(201, scim_adapter.scim_resource(scim_adapter.scim_create!(user)))
        rescue FogClient::Signin::SCIM::BadRequest => e
          scim_render(400, FogClient::Signin::SCIM.error(400, e.message))
        end

        def replace
          return unless scim_authorized!

          account = scim_adapter.scim_find(params[:id].to_s)
          return scim_render(404, FogClient::Signin::SCIM.error(404, "no such user")) unless account

          user = FogClient::Signin::SCIM.parse_user(scim_body)
          scim_render(200, scim_adapter.scim_resource(scim_adapter.scim_replace!(account, user)))
        rescue FogClient::Signin::SCIM::BadRequest => e
          scim_render(400, FogClient::Signin::SCIM.error(400, e.message))
        end

        def patch
          return unless scim_authorized!

          account = scim_adapter.scim_find(params[:id].to_s)
          return scim_render(404, FogClient::Signin::SCIM.error(404, "no such user")) unless account

          current = scim_adapter.scim_resource(account)
          active = FogClient::Signin::SCIM.active_from_patch(scim_body)
          user = FogClient::Signin::SCIM.parse_user(current.merge("active" => active))
          scim_render(200, scim_adapter.scim_resource(scim_adapter.scim_replace!(account, user)))
        rescue FogClient::Signin::SCIM::BadRequest => e
          scim_render(400, FogClient::Signin::SCIM.error(400, e.message))
        end

        private

        # Without SCIM_TOKEN the endpoint does not exist; with it, the
        # bearer must match, compared in constant time.
        def scim_authorized!
          if ENV["SCIM_TOKEN"].to_s.empty?
            head :not_found
            return false
          end
          return true if FogClient::Signin::SCIM.authorized?(request.headers["Authorization"])

          scim_render(401, FogClient::Signin::SCIM.error(401, "unauthorized"))
          false
        end

        def scim_body
          JSON.parse(request.raw_post.presence || "{}")
        rescue JSON::ParserError
          raise FogClient::Signin::SCIM::BadRequest, "body is not JSON"
        end

        def scim_render(status, body)
          response.headers["Cache-Control"] = "no-store"
          render json: body, status: status, content_type: "application/scim+json"
        end
      end
    end
  end
end
