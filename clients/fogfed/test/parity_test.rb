# frozen_string_literal: true

require "minitest/autorun"
require "json"
require "fogfed"

# The conformance vectors fogd's Go checks too (internal/fed,
# TestFogfed1ParserParity): what Go's RFC 3339 parse and its envelope head
# decoders take and refuse, this gem takes and refuses.
class FogfedParityTest < Minitest::Test
  VECTORS = JSON.parse(File.read(File.join(__dir__, "..", "conformance", "vectors.json")))

  def frame(head) = [head.bytesize].pack("N") + head + "body".b

  def test_timestamps_accepted
    VECTORS.dig("timestamps", "accept").each do |ts|
      Fogfed::Signature.parse_timestamp(ts)
    rescue ArgumentError => e
      flunk "#{ts.inspect} refused: #{e.message}"
    end
  end

  # 24:00:00, a leap second and 30 February among them: Time.iso8601 takes all three.
  def test_timestamps_refused
    VECTORS.dig("timestamps", "reject").each do |ts|
      assert_raises(ArgumentError, ts.inspect) { Fogfed::Signature.parse_timestamp(ts) }
    end
  end

  # A refused timestamp never reaches the key: verify! says so, signed or not.
  def test_a_refused_timestamp_does_not_verify
    id = Fogfed::Identity.generate
    pub = Fogfed.parse_public_key(id.public_key_b64)
    ["2026-01-02T24:00:00Z", "2026-01-02T23:59:60Z", "2026-02-30T00:00:00Z"].each do |ts|
      assert_raises(Fogfed::Signature::BadSignature, ts) do
        Fogfed::Signature.verify!(pub, "GET", "/", ts, id.sign("GET", "/", ts), "", now: Time.utc(2026, 1, 2))
      end
    end
  end

  def test_parsed_instants
    assert_equal Time.utc(2026, 1, 2, 2, 4, 5) + Rational(1, 2), Fogfed::Signature.parse_timestamp("2026-01-02T3:04:05,5+01:00")
    assert_equal Time.utc(2026, 1, 2, 3, 4, 5), Fogfed::Signature.parse_timestamp("2026-01-02T03:04:05Z")
  end

  # Every malformed head is an Envelope::Error, never a TypeError or
  # NoMethodError from inside the decoder.
  def test_request_heads_refused
    VECTORS.dig("envelope_rejections", "requests").each do |c|
      assert_raises(Fogfed::Envelope::Error, c["name"]) do
        Fogfed::Envelope.decode_request(frame(Fogfed::B64.decode(c["head_b64"])))
      end
    end
  end

  def test_answer_heads_refused
    VECTORS.dig("envelope_rejections", "answers").each do |c|
      assert_raises(Fogfed::Envelope::Error, c["name"]) { Fogfed::Envelope.parse_answer_head(Fogfed::B64.decode(c["head_b64"])) }
    end
  end

  # decode_answer refuses a bad head before it looks at a signature.
  def test_decode_answer_refuses_a_bad_head_first
    pub = Fogfed.parse_public_key(Fogfed::Identity.generate.public_key_b64)
    ["[1]", '{"status":"200"}', '{"status":200,"headers":{"A":"v"}}'].each do |h|
      plain = [h.bytesize].pack("N") + h + [0].pack("n")
      assert_raises(Fogfed::Envelope::Error, h) { Fogfed::Envelope.decode_answer(plain, pub, "/x", "", "") }
    end
  end

  def test_request_heads_accepted
    VECTORS.dig("envelope_accepts", "requests").each do |c|
      method, target, headers, body = Fogfed::Envelope.decode_request(frame(Fogfed::B64.decode(c["head_b64"])))
      assert_equal [c["method"], c["target"], c["headers"], "body"], [method, target, headers, body], c["name"]
    end
  end

  def test_answer_heads_accepted
    VECTORS.dig("envelope_accepts", "answers").each do |c|
      assert_equal c["status"], Fogfed::Envelope.parse_answer_head(Fogfed::B64.decode(c["head_b64"]))["status"], c["name"]
    end
  end

  # The one place this is stricter than Go: two keys for one field.
  def test_two_keys_for_one_field_are_refused
    ['{"method":"GET","method":"PUT","target":"/x"}', '{"method":"GET","Method":"PUT","target":"/x"}'].each do |h|
      assert_raises(Fogfed::Envelope::Error, h) { Fogfed::Envelope.decode_request(frame(h)) }
    end
  end
end
