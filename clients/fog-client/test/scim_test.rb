# frozen_string_literal: true

require "minitest/autorun"
require_relative "../lib/fog_client"
require_relative "../lib/fog_client/signin/scim"

class SCIMParseTest < Minitest::Test
  S = FogClient::Signin::SCIM

  def user(extra = {})
    { "schemas" => [S::USER_SCHEMA], "externalId" => "person:H/KIM", "userName" => "kim", "displayName" => "Kim",
      "active" => true, "roles" => [{ "value" => "member", "primary" => true }],
      "emails" => [{ "value" => "kim@example.com", "primary" => true }] }.merge(extra)
  end

  def test_parse_user
    u = S.parse_user(user)
    assert_equal ["person:H/KIM", "kim", "Kim", true, "kim@example.com", "member"],
                 [u.external_id, u.user_name, u.display_name, u.active, u.email, u.role]
    assert_equal "kim", S.parse_user(user("displayName" => "")).display_name
    refute S.parse_user(user("active" => false)).active
    assert_raises(S::BadRequest) { S.parse_user(user("schemas" => [])) }
    assert_raises(S::BadRequest) { S.parse_user(user("externalId" => "")) }
  end

  def test_filter_is_externalid_only
    assert_nil S.parse_filter(nil)
    assert_equal "person:H/KIM", S.parse_filter('externalId eq "person:H/KIM"')
    assert_equal 'a"b', S.parse_filter('externalId eq "a\\"b"')
    assert_raises(S::BadRequest) { S.parse_filter('userName eq "kim"') }
    assert_raises(S::BadRequest) { S.parse_filter('externalId eq "x" or 1 eq 1') }
  end

  def test_patch_active_only
    patch = ->(op) { { "schemas" => [S::PATCH_SCHEMA], "Operations" => [op] } }
    refute S.active_from_patch(patch.call("op" => "replace", "path" => "active", "value" => false))
    assert S.active_from_patch(patch.call("op" => "Replace", "value" => { "active" => true }))
    assert_raises(S::BadRequest) { S.active_from_patch(patch.call("op" => "replace", "path" => "userName", "value" => "x")) }
    assert_raises(S::BadRequest) { S.active_from_patch(patch.call("op" => "remove", "path" => "active")) }
  end

  def test_authorized
    env = { "SCIM_TOKEN" => "t0ken" }
    assert S.authorized?("Bearer t0ken", env)
    refute S.authorized?("Bearer t0ke", env)
    refute S.authorized?("t0ken", env)
    refute S.authorized?("Bearer ", {})
  end
end
