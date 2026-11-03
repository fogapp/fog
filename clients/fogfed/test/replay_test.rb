# frozen_string_literal: true

require "minitest/autorun"
require "fogfed"

# A captured request, sent again inside the ±MAX_SKEW window, verifies again
# unless verify! is given a seen: record.
class FogfedReplayTest < Minitest::Test
  AT = Time.utc(2026, 1, 2, 3, 4, 5)

  def setup
    @id = Fogfed::Identity.generate
    @pub = Fogfed.parse_public_key(@id.public_key_b64)
    @ts = Fogfed::Signature.stamp(AT)
  end

  def signed(body = '{"pay":1}', ts = @ts) = [@pub, "POST", "/fed/v1/transfer", ts, @id.sign("POST", "/fed/v1/transfer", ts, body), body]

  def test_without_seen_a_replay_verifies_again
    req = signed
    assert Fogfed::Signature.verify!(*req, now: AT)
    assert Fogfed::Signature.verify!(*req, now: AT + 60)
  end

  def test_seen_refuses_the_replay
    cache = Fogfed::Signature::ReplayCache.new
    req = signed
    assert Fogfed::Signature.verify!(*req, now: AT, seen: cache)
    assert_raises(Fogfed::Signature::Replayed) { Fogfed::Signature.verify!(*req, now: AT + 60, seen: cache) }
    refute Fogfed::Signature.verify(*req, now: AT + 61, seen: cache)
    # Another request, even an identical body at another stamp, is new.
    assert Fogfed::Signature.verify!(*signed('{"pay":1}', Fogfed::Signature.stamp(AT + 1)), now: AT, seen: cache)
  end

  # Only a verified signature is recorded: a forgery can't fill the store,
  # and a request that fails another check isn't marked as seen.
  def test_seen_is_asked_only_after_the_signature_verifies
    calls = []
    spy = ->(key, exp) { calls << [key, exp]; false }
    forged = signed
    forged[4] = Fogfed::Identity.generate.sign("POST", "/fed/v1/transfer", @ts, '{"pay":1}')
    refute Fogfed::Signature.verify(*forged, now: AT, seen: spy)
    refute Fogfed::Signature.verify(*signed, now: AT + 3600, seen: spy) # skew
    assert_empty calls
    req = signed
    Fogfed::Signature.verify!(*req, now: AT, seen: spy)
    assert_equal [[req[4], Fogfed::Signature.parse_timestamp(@ts) + Fogfed::Signature::MAX_SKEW]], calls
  end

  # Any callable works: a shared store for an app with several processes.
  def test_a_shared_store_as_seen
    store = {}
    seen = ->(key, exp) { store.key?(key) ? true : (store[key] = exp; false) }
    req = signed
    assert Fogfed::Signature.verify(*req, now: AT, seen: seen)
    refute Fogfed::Signature.verify(*req, now: AT, seen: seen)
  end

  # Full of live signatures, the cache refuses rather than forget one that
  # could still be replayed; once they expire it takes new ones again.
  def test_a_full_cache_fails_closed_then_recovers
    cache = Fogfed::Signature::ReplayCache.new(max: 2)
    assert_equal false, cache.call("a", AT + 300, now: AT)
    assert_equal false, cache.call("b", AT + 300, now: AT)
    assert_equal true, cache.call("c", AT + 300, now: AT)
    assert_equal true, cache.call("a", AT + 300, now: AT + 100)
    assert_equal false, cache.call("c", AT + 700, now: AT + 400)
  end
end
