# frozen_string_literal: true

require "minitest/autorun"
require "json"
require "time"
require "fogfed"

class FogfedTest < Minitest::Test
  VECTORS = JSON.parse(File.read(File.join(__dir__, "..", "conformance", "vectors.json")))
  AT = Time.iso8601("2026-01-02T03:04:05Z")

  def test_signature_known_answers
    VECTORS.fetch("signatures").each do |v|
      id = Fogfed::Identity.from_seed(instance_id: "x", seed: v["seed_b64"])
      assert_equal v["public_key_b64"], id.public_key_b64, "#{v['name']}: public key"
      assert_equal v["signing_string"], Fogfed::Signature.signing_string(v["method"], v["target"], v["timestamp"], v["body"]), "#{v['name']}: signing string"
      assert_equal v["signature_b64url"], id.sign(v["method"], v["target"], v["timestamp"], v["body"]), "#{v['name']}: signature"

      pub = Fogfed.parse_public_key(v["public_key_b64"])
      assert Fogfed::Signature.verify!(pub, v["method"], v["target"], v["timestamp"], v["signature_b64url"], v["body"], now: AT), "#{v['name']}: verify"
    end
  end

  def test_transcript_known_answers
    VECTORS.fetch("transcripts").each do |v|
      id = Fogfed::Identity.from_seed(instance_id: "x", seed: v["seed_b64"])
      assert_equal v["signature_b64url"], id.sign_transcript(v["label"], v["nonce"], v["doc_json"]), "#{v['name']}: transcript sig"
      pub = Fogfed.parse_public_key(v["public_key_b64"])
      assert Fogfed::Signature.verify_transcript(pub, v["label"], v["nonce"], v["doc_json"], v["signature_b64url"]), "#{v['name']}: transcript verify"
    end
  end

  def test_answer_known_answers
    VECTORS.fetch("answers").each do |v|
      id = Fogfed::Identity.from_seed(instance_id: "x", seed: v["seed_b64"])
      material = Fogfed::Signature.answer_material(v["status"], v["request_instance"], v["request_signature"], v["body"])
      assert_equal v["signing_string"], Fogfed::Signature.signing_string("ANSWER", v["target"], v["timestamp"], material), "#{v['name']}: signing string"
      assert_equal v["signature_b64url"], id.sign_answer(v["target"], v["timestamp"], v["status"], v["request_instance"], v["request_signature"], v["body"]), "#{v['name']}: signature"
      pub = Fogfed.parse_public_key(v["public_key_b64"])
      at = Time.iso8601(v["timestamp"])
      assert Fogfed::Signature.verify_answer!(pub, v["target"], v["timestamp"], v["signature_b64url"], v["status"],
                                              v["request_instance"], v["request_signature"], v["body"], now: at), "#{v['name']}: verify"
    end
  end

  # The envelope, both ways: the request opens to its method, target,
  # headers and body; the answer opens, its head verifies for that request,
  # and its body matches the head's digest.
  def test_envelope_known_answers
    VECTORS.fetch("envelopes").each do |v|
      requester = Fogfed::Identity.from_seed(instance_id: v["requester_instance"], seed: v["requester_seed_b64"])
      responder = Fogfed::Identity.from_seed(instance_id: v["responder_instance"], seed: v["responder_seed_b64"])
      outer = Fogfed::Envelope::SEALED_PATH
      req_plain = Fogfed::Seal.open(Fogfed::B64.decode(v["responder_enc_private_b64"]),
                                    Fogfed::Seal.request_info("POST", outer, requester.instance_id, responder.instance_id, v["timestamp"]),
                                    Fogfed::B64.decode(v["request_sealed_b64"]))
      method, target, headers, body = Fogfed::Envelope.decode_request(req_plain)
      assert_equal [v["method"], v["target"], v["request_headers"] || {}, Fogfed::B64.decode(v["request_body_b64"])], [method, target, headers, body], v["name"]
      assert Fogfed::Signature.verify!(Fogfed.parse_public_key(requester.public_key_b64), "POST", outer, v["timestamp"],
                                       v["request_signature"], Fogfed::B64.decode(v["request_sealed_b64"]), now: Time.iso8601(v["timestamp"]))
      ans_plain = Fogfed::Seal.open(Fogfed::B64.decode(v["requester_enc_private_b64"]),
                                    Fogfed::Seal.answer_info("POST", outer, requester.instance_id, v["request_signature"], responder.instance_id),
                                    Fogfed::B64.decode(v["answer_sealed_b64"]))
      head, abody = Fogfed::Envelope.decode_answer(ans_plain, Fogfed.parse_public_key(responder.public_key_b64), outer,
                                                   requester.instance_id, v["request_signature"], now: Time.iso8601("2026-01-02T03:04:06Z"))
      assert_equal v["status"], head["status"], v["name"]
      assert_equal Fogfed::B64.decode(v["answer_body_b64"]), abody, v["name"]
      assert_raises(Fogfed::Signature::BadSignature, v["name"]) do
        Fogfed::Envelope.decode_answer(ans_plain, Fogfed.parse_public_key(responder.public_key_b64), outer,
                                       requester.instance_id, "another-request", now: Time.iso8601("2026-01-02T03:04:06Z"))
      end
    end
  end

  # An answer verifies for the request it answers and no other: not another
  # request's signature, not another requester, not another status, and
  # never as a request or an unbound RESPONSE.
  def test_answer_is_bound_to_its_request
    id = Fogfed::Identity.generate
    pub = Fogfed.parse_public_key(id.public_key_b64)
    ts = Fogfed::Signature.now_timestamp
    sig = id.sign_answer("/fed/v1/heartbeat", ts, 200, "01REQ", "reqsig", '{"ok":true}')
    assert Fogfed::Signature.verify_answer!(pub, "/fed/v1/heartbeat", ts, sig, 200, "01REQ", "reqsig", '{"ok":true}')
    refute Fogfed::Signature.verify(pub, "ANSWER", "/fed/v1/heartbeat", ts, sig, Fogfed::Signature.answer_material(200, "01REQ", "later", '{"ok":true}'))
    refute Fogfed::Signature.verify(pub, "ANSWER", "/fed/v1/heartbeat", ts, sig, Fogfed::Signature.answer_material(200, "01OTHER", "reqsig", '{"ok":true}'))
    refute Fogfed::Signature.verify(pub, "ANSWER", "/fed/v1/heartbeat", ts, sig, Fogfed::Signature.answer_material(403, "01REQ", "reqsig", '{"ok":true}'))
    refute Fogfed::Signature.verify(pub, "RESPONSE", "/fed/v1/heartbeat", ts, sig, '{"ok":true}')
    refute Fogfed::Signature.verify(pub, "POST", "/fed/v1/heartbeat", ts, sig, '{"ok":true}')
  end

  # Two requests stamped in the same instant are still different bytes.
  def test_request_stamps_never_repeat
    at = Time.now
    stamps = Array.new(1000) { Fogfed::Signature.now_timestamp }
    assert_equal stamps.uniq.length, stamps.length
    stamps.each { |ts| assert Fogfed::Signature::RFC3339.match?(ts), ts }
    id = Fogfed::Identity.generate
    pub = Fogfed.parse_public_key(id.public_key_b64)
    ts = stamps.last
    assert Fogfed::Signature.verify!(pub, "GET", "/x", ts, id.sign("GET", "/x", ts), "", now: at)
  end

  # Sealed bodies, byte for byte: the same seeds give the same bytes, which
  # open to the same plaintext (Go's crypto/hpke opens these exact bytes in
  # its conformance test).
  def test_seal_known_answers
    VECTORS.fetch("seals").each do |v|
      sk = Fogfed::B64.decode(v["recipient_private_b64"])
      pk = Fogfed::B64.decode(v["recipient_public_b64"])
      assert_equal [sk, pk], Fogfed::Seal.derive_key_pair(Fogfed::B64.decode(v["recipient_seed_b64"])), "#{v['name']}: keys"
      plain = Fogfed::B64.decode(v["plaintext_b64"])
      sealed = Fogfed::B64.decode(v["sealed_b64"])
      assert_equal sealed, Fogfed::Seal.seal(pk, v["info"], plain, ikm_e: Fogfed::B64.decode(v["ephemeral_seed_b64"])), "#{v['name']}: sealed bytes"
      assert_equal plain, Fogfed::Seal.open(sk, v["info"], sealed), "#{v['name']}: opens"
    end
  end

  def test_encryption_binding_known_answers
    VECTORS.fetch("encryption_bindings").each do |v|
      id = Fogfed::Identity.from_seed(instance_id: v["instance_id"], seed: v["seed_b64"])
      assert_equal v["binding_b64url"], id.bind_encryption_key(Fogfed::B64.decode(v["encryption_key_b64"]))
      pub = Fogfed.parse_public_key(v["public_key_b64"])
      assert Fogfed.verify_encryption_binding(pub, v["instance_id"], v["encryption_key_b64"], v["binding_b64url"])
      refute Fogfed.verify_encryption_binding(pub, "01OTHER", v["encryption_key_b64"], v["binding_b64url"])
    end
  end

  # A sealed body opens only whole, untouched, in order, for its exchange.
  def test_sealed_body_opens_only_as_sealed
    sk, pk = Fogfed::Seal.derive_key_pair(OpenSSL::Random.random_bytes(32))
    info = Fogfed::Seal.request_info("POST", "/fed/v1/sealed", "01A", "01B", "2026-01-02T03:04:05.000000001Z")
    plain = "y" * (2 * Fogfed::Seal::CHUNK + 10)
    sealed = Fogfed::Seal.seal(pk, info, plain)
    assert_equal plain.b, Fogfed::Seal.open(sk, info, sealed)
    head = sealed.byteslice(0, 33)
    recs = []
    pos = 33
    while pos < sealed.bytesize
      n = sealed.byteslice(pos, 4).unpack1("N")
      recs << sealed.byteslice(pos, 4 + n)
      pos += 4 + n
    end
    flipped = sealed.dup
    flipped.setbyte(sealed.bytesize / 2, flipped.getbyte(sealed.bytesize / 2) ^ 1)
    {
      "flipped" => flipped,
      "cut" => head + recs[0] + recs[1],
      "reordered" => head + recs[1] + recs[0] + recs[2],
      "trailing" => sealed + recs[0]
    }.each do |name, bad|
      assert_raises(Fogfed::Seal::Error, name) { Fogfed::Seal.open(sk, info, bad) }
    end
    other = Fogfed::Seal.request_info("POST", "/fed/v1/sealed", "01A", "01B", "2026-01-02T03:04:05.000000002Z")
    assert_raises(Fogfed::Seal::Error) { Fogfed::Seal.open(sk, other, sealed) }
  end

  # Every other spelling of a canonical signature or key — the spare bits
  # of the last character set — is refused, as fogd refuses them.
  def test_sealed_targets_name_their_recipient
    targets = VECTORS.fetch("sealed_targets")
    refute_empty targets
    targets.each { |v| assert_equal v["target"], Fogfed::Envelope.sealed_target(v["recipient"]) }
  end

  def test_instance_ids_are_derived_from_keys
    ids = VECTORS.fetch("instance_ids")
    refute_empty ids
    ids.each_with_index do |v, i|
      raw = Fogfed::B64.decode(v["public_key_b64"])
      assert_equal v["instance_id"], Fogfed::Identity.derive_instance_id(raw)
      assert Fogfed.check_instance_id(v["instance_id"], v["public_key_b64"])
      refute Fogfed.check_instance_id(v["instance_id"], ids[(i + 1) % ids.length]["public_key_b64"])
    end
    assert Fogfed.check_instance_id("01JV1CT1M0000000000000000A", ids[0]["public_key_b64"]), "legacy ULIDs are taken as given"
    refute Fogfed.check_instance_id("ZZZZ", ids[0]["public_key_b64"])
    fresh = Fogfed::Identity.generate
    assert Fogfed.check_instance_id(fresh.instance_id, fresh.public_key_b64)
    assert_match(/\A[GHJKMNPQ]/, fresh.instance_id)
  end

  # A malformed answer head — not an object, or without a status — is the
  # gem's own error, never a crash in the code reading it.
  def test_malformed_answer_heads_raise_the_gems_error
    pub = Fogfed.parse_public_key(Fogfed::Identity.generate.public_key_b64)
    ["[]", "null", "1", '"x"', '{"headers":{},"digest":"","instance":"i","ts":"t"}', '{"status":"200"}', '{"status":99}'].each do |head|
      plain = [head.bytesize].pack("N") + head + [0].pack("n")
      assert_raises(Fogfed::Envelope::Error, head) do
        Fogfed::Envelope.decode_answer(plain, pub, "/fed/v1/sealed", "01JREQ", "c2ln")
      end
    end
  end

  def test_noncanonical_encodings_are_refused
    v = VECTORS.fetch("noncanonical")
    pub = Fogfed.parse_public_key(v["public_key_b64"])
    at = Time.iso8601(v["timestamp"])
    assert Fogfed::Signature.verify!(pub, "POST", v["target"], v["timestamp"], v["signature_b64url"], v["body"], now: at)
    assert_equal 15, v["signatures"].length
    v["signatures"].each do |sig|
      refute Fogfed::Signature.verify(pub, "POST", v["target"], v["timestamp"], sig, v["body"], now: at), sig
    end
    assert_equal 3, v["public_keys"].length
    v["public_keys"].each do |k|
      assert_raises(ArgumentError, k) { Fogfed.parse_public_key(k) }
    end
  end

  def test_sign_verify_round_trip
    id = Fogfed::Identity.generate
    ts = Fogfed::Signature.now_timestamp
    sig = id.sign("POST", "/fed/v1/heartbeat", ts, '{"a":1}')
    pub = Fogfed.parse_public_key(id.public_key_b64)
    assert Fogfed::Signature.verify!(pub, "POST", "/fed/v1/heartbeat", ts, sig, '{"a":1}')
  end

  def test_query_is_covered
    id = Fogfed::Identity.generate
    ts = Fogfed::Signature.now_timestamp
    sig = id.sign("DELETE", "/c?path=a.jpg", ts, "")
    pub = Fogfed.parse_public_key(id.public_key_b64)
    assert Fogfed::Signature.verify!(pub, "DELETE", "/c?path=a.jpg", ts, sig, "")
    refute Fogfed::Signature.verify(pub, "DELETE", "/c?path=other.jpg", ts, sig, "")
    refute Fogfed::Signature.verify(pub, "DELETE", "/c", ts, sig, "")
  end

  def test_body_tamper_fails
    id = Fogfed::Identity.generate
    ts = Fogfed::Signature.now_timestamp
    sig = id.sign("POST", "/x", ts, "one")
    pub = Fogfed.parse_public_key(id.public_key_b64)
    refute Fogfed::Signature.verify(pub, "POST", "/x", ts, sig, "two")
  end

  def test_skew_rejected
    id = Fogfed::Identity.generate
    ts = Fogfed::Signature.now_timestamp(Time.now - 3600)
    sig = id.sign("GET", "/x", ts, "")
    pub = Fogfed.parse_public_key(id.public_key_b64)
    assert_raises(Fogfed::Signature::SkewError) do
      Fogfed::Signature.verify!(pub, "GET", "/x", ts, sig, "")
    end
  end

  def test_wrong_key_rejected
    id = Fogfed::Identity.generate
    ts = Fogfed::Signature.now_timestamp
    sig = id.sign("GET", "/x", ts, "")
    other = Fogfed.parse_public_key(Fogfed::Identity.generate.public_key_b64)
    refute Fogfed::Signature.verify(other, "GET", "/x", ts, sig, "")
  end

  def test_ulid_shape_and_order
    a = Fogfed::Ulid.generate(time_ms: 1_700_000_000_000)
    b = Fogfed::Ulid.generate(time_ms: 1_700_000_001_000)
    assert_equal 26, a.length
    assert_match(/\A[0-9A-HJKMNP-TV-Z]{26}\z/, a)
    assert a < b, "later ULID should sort after earlier"
  end

  def test_identity_seed_round_trip
    id = Fogfed::Identity.generate
    again = Fogfed::Identity.from_seed(instance_id: id.instance_id, seed: id.seed_b64)
    assert_equal id.public_key_b64, again.public_key_b64
    assert_equal id.seed_b64, again.seed_b64
  end

  def test_instance_doc_shape
    id = Fogfed::Identity.from_seed(instance_id: "01JXX", seed: [1].pack("C") * 32)
    doc = id.instance_doc(name: "dave", url: "https://fog.dave.house", version: "0.3.0")
    assert_equal %w[instanceId name publicKey url fogVersion].sort, doc.keys.sort
    assert_equal "dave", doc["name"]
    refute id.instance_doc(name: "x").key?("url")
  end

  # base64 left the default gems in Ruby 3.4: under Bundler it loads only if
  # the app's Gemfile names it. --disable-gems leaves it just as unloadable.
  def test_loads_without_the_base64_gem
    lib = File.expand_path("../lib", __dir__)
    out = IO.popen([RbConfig.ruby, "--disable-gems", "-I", lib, "-e",
      'begin; require "base64"; puts "base64 loadable"; rescue LoadError; end; require "fogfed"; ' \
      'id = Fogfed::Identity.generate; ts = Fogfed::Signature.now_timestamp; ' \
      'puts Fogfed::Signature.verify(Fogfed.parse_public_key(id.public_key_b64), "GET", "/", ts, id.sign("GET", "/", ts))'],
      err: [:child, :out], &:read)
    assert_equal "true\n", out
  end

  # Go decodes signatures with base64.RawURLEncoding and keys with
  # StdEncoding: padding, the other alphabet or junk are refused in both.
  def test_encodings_are_as_strict_as_go
    id = Fogfed::Identity.generate
    ts = Fogfed::Signature.now_timestamp
    sig = id.sign("GET", "/x", ts, "")
    pub = Fogfed.parse_public_key(id.public_key_b64)
    refute_includes sig, "="
    [sig + "==", sig.tr("-_", "+/") + "+", " #{sig}", "#{sig}\n", sig[0..-2] + "*"].each do |bad|
      assert_raises(Fogfed::Signature::BadSignature, bad.inspect) { Fogfed::Signature.verify!(pub, "GET", "/x", ts, bad, "") }
    end
    refute Fogfed::Signature.verify_transcript(pub, "l", "n", "{}", "#{id.sign_transcript("l", "n", "{}")}==")
    assert Fogfed::Signature.verify_transcript(pub, "l", "n", "{}", id.sign_transcript("l", "n", "{}"))
    [id.public_key_b64.delete("="), id.public_key_b64.tr("+/", "-_") + "\n", "#{id.public_key_b64}\n"].each do |bad|
      next if bad == id.public_key_b64
      assert_raises(ArgumentError, bad.inspect) { Fogfed.parse_public_key(bad) }
    end
    assert_equal id.seed, Fogfed::B64.decode(id.seed_b64)
    assert_equal "\xFB\xFF".b, Fogfed::B64.urlsafe_decode("-_8")
  end

  # The key decides the algorithm, so only Ed25519 keys are accepted: an RSA
  # or EC key is a programming error, never a quiet false or a verification
  # under another scheme.
  def test_only_ed25519_keys
    id = Fogfed::Identity.generate
    ts = Fogfed::Signature.now_timestamp
    sig = id.sign("GET", "/x", ts, "")
    [OpenSSL::PKey::RSA.new(2048), OpenSSL::PKey::EC.generate("prime256v1"), "key", nil].each do |key|
      assert_raises(ArgumentError) { Fogfed::Signature.verify(key, "GET", "/x", ts, sig, "") }
      assert_raises(ArgumentError) { Fogfed::Signature.verify_transcript(key, "l", "n", "{}", sig) }
      assert_raises(ArgumentError) { Fogfed::Signature.sign(key, "GET", "/x", ts, "") }
    end
  end

  # Timestamps read as Go's time.Parse(time.RFC3339) reads them, and the
  # window is ±MAX_SKEW either side of now.
  def test_timestamps_and_skew
    id = Fogfed::Identity.generate
    pub = Fogfed.parse_public_key(id.public_key_b64)
    now = Time.utc(2026, 1, 2, 3, 4, 5)
    ok = ->(ts, at = now) { Fogfed::Signature.verify!(pub, "GET", "/", ts, id.sign("GET", "/", ts), "", now: at) }
    assert ok.("2026-01-02T03:04:05Z")
    assert ok.("2026-01-02T03:04:05.123Z")
    assert ok.("2026-01-02T04:04:05+01:00")
    assert ok.("2026-01-02T03:09:05Z")
    assert ok.("2026-01-02T02:59:05Z")
    assert_raises(Fogfed::Signature::SkewError) { ok.("2026-01-02T03:09:06Z") }
    assert_raises(Fogfed::Signature::SkewError) { ok.("2026-01-02T02:59:04Z") }
    ["2026-01-02T03:04Z", "2026-01-02", "2026-01-02 03:04:05Z", "2026-01-02T03:04:05+0100", "", nil, 1_767_323_045].each do |ts|
      assert_raises(Fogfed::Signature::BadSignature, ts.inspect) do
        Fogfed::Signature.verify!(pub, "GET", "/", ts, id.sign("GET", "/", ts.to_s), "", now: now)
      end
    end
  end

  def test_parse_public_key_rejects_bad_length
    assert_raises(ArgumentError) { Fogfed.parse_public_key(["short"].pack("m0")) }
  end
end

class FogfedImpactTest < Minitest::Test
  VECTORS = JSON.parse(File.read(File.join(__dir__, "..", "conformance", "vectors.json")))

  def test_impact_known_answers
    VECTORS.fetch("impacts").each do |v|
      got = Fogfed::Impact.sentence(subject: v["subject"], scope: v["scope"], sees: v["sees"],
        hidden: v["hidden"], floor: v["floor"], addendum: v["addendum"])
      assert_equal v["sentence"], got, v["name"]
    end
  end

  def test_to_sentence
    assert_equal "", Fogfed::Impact.to_sentence([])
    assert_equal "a", Fogfed::Impact.to_sentence(["a"])
    assert_equal "a and b", Fogfed::Impact.to_sentence(%w[a b])
    assert_equal "a, b, and c", Fogfed::Impact.to_sentence(%w[a b c])
    assert_equal "a and c", Fogfed::Impact.to_sentence(["a", "", "c"])
  end
end

class FogfedLensTest < Minitest::Test
  VECTORS = JSON.parse(File.read(File.join(__dir__, "..", "conformance", "vectors.json")))

  # The gem's vocabulary is exactly what its own conformance vectors carry —
  # the same list Go asserts against, so all three (gem, fogd, vectors) agree.
  def test_lens_matches_vectors
    lens = VECTORS.fetch("lens")
    assert_equal lens.keys.sort, Fogfed::Lens::AXES.keys.sort
    lens.each do |axis, terms|
      got = Fogfed::Lens.axis(axis).map { |t| { "value" => t.value, "label" => t.label } }
      assert_equal terms, got, axis
    end
  end

  def test_lens_helpers
    assert Fogfed::Lens.valid?("mode", "contribute")
    refute Fogfed::Lens.valid?("mode", "delete")
    assert_equal %w[all filtered picked], Fogfed::Lens.values("scope")
    assert_equal "withheld", Fogfed::Lens.label("provenance", "withheld")
    assert_equal "read and contribute", Fogfed::Lens.label("mode", "contribute")
    assert_equal "mystery", Fogfed::Lens.label("mode", "mystery")
    assert_raises(KeyError) { Fogfed::Lens.axis("nonsense") }
  end
end
