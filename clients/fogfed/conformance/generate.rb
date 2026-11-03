# frozen_string_literal: true

# Regenerates vectors.json. Ed25519 is deterministic (RFC 8032), so a fixed
# seed and message always yield the same signature; these known-answer
# vectors are what fogd's Go and this gem both verify against.
require_relative "../lib/fogfed"
require "json"

def seed(byte) = [byte].pack("C") * 32

def sig_case(name, byte, method, target, timestamp, body)
  id = Fogfed::Identity.from_seed(instance_id: "01JCONFORMANCEVECTOR000001", seed: seed(byte))
  {
    "name" => name, "seed_b64" => id.seed_b64, "public_key_b64" => id.public_key_b64,
    "method" => method, "target" => target, "timestamp" => timestamp, "body" => body,
    "signing_string" => Fogfed::Signature.signing_string(method, target, timestamp, body),
    "signature_b64url" => id.sign(method, target, timestamp, body)
  }
end

def transcript_case(name, byte, label, nonce, doc)
  id = Fogfed::Identity.from_seed(instance_id: "01JCONFORMANCEVECTOR000001", seed: seed(byte))
  doc_json = JSON.generate(doc)
  {
    "name" => name, "seed_b64" => id.seed_b64, "public_key_b64" => id.public_key_b64,
    "label" => label, "nonce" => nonce, "doc_json" => doc_json,
    "signature_b64url" => id.sign_transcript(label, nonce, doc_json)
  }
end


def answer_case(name, byte, target, timestamp, status, req_instance, req_sig, body)
  id = Fogfed::Identity.from_seed(instance_id: "01JCONFORMANCEVECTOR000001", seed: seed(byte))
  {
    "name" => name, "seed_b64" => id.seed_b64, "public_key_b64" => id.public_key_b64,
    "target" => target, "timestamp" => timestamp, "status" => status,
    "request_instance" => req_instance, "request_signature" => req_sig, "body" => body,
    "signing_string" => Fogfed::Signature.signing_string(Fogfed::Signature::ANSWER_METHOD, target, timestamp,
      Fogfed::Signature.answer_material(status, req_instance, req_sig, body)),
    "signature_b64url" => id.sign_answer(target, timestamp, status, req_instance, req_sig, body)
  }
end

# One exchange in the envelope, both ways, with fixed seeds: a request
# sealed to a responder, and the responder's answer sealed back, its head
# signed. Go opens and verifies both.
def envelope_case(name, requester_byte, responder_byte, eph_req_byte, eph_ans_byte, method, target, req_headers, req_body,
                  status, ans_headers, ans_body)
  requester = Fogfed::Identity.from_seed(instance_id: "01JREQUESTER0000000000000A", seed: seed(requester_byte))
  responder = Fogfed::Identity.from_seed(instance_id: "01JRESPONDER0000000000000B", seed: seed(responder_byte))
  req_sk, req_pk = Fogfed::Seal.derive_key_pair(seed(requester_byte + 0x40))
  resp_sk, resp_pk = Fogfed::Seal.derive_key_pair(seed(responder_byte + 0x40))
  outer = Fogfed::Envelope::SEALED_PATH
  ts = "2026-01-02T03:04:05.000000001Z"
  req_info = Fogfed::Seal.request_info("POST", outer, requester.instance_id, responder.instance_id, ts)
  req_sealed = Fogfed::Seal.seal(resp_pk, req_info, Fogfed::Envelope.encode_request(method, target, req_headers, req_body),
                                 ikm_e: seed(eph_req_byte))
  req_sig = requester.sign("POST", outer, ts, req_sealed)
  ans_ts = "2026-01-02T03:04:06.000000002Z"
  ans_plain = Fogfed::Envelope.encode_answer(responder, outer, ans_ts, requester.instance_id, req_sig, status, ans_headers, ans_body)
  ans_info = Fogfed::Seal.answer_info("POST", outer, requester.instance_id, req_sig, responder.instance_id)
  {
    "name" => name,
    "requester_seed_b64" => requester.seed_b64, "requester_instance" => requester.instance_id,
    "responder_seed_b64" => responder.seed_b64, "responder_instance" => responder.instance_id,
    "requester_enc_private_b64" => Fogfed::B64.encode(req_sk), "responder_enc_private_b64" => Fogfed::B64.encode(resp_sk),
    "timestamp" => ts, "request_signature" => req_sig,
    "request_sealed_b64" => Fogfed::B64.encode(req_sealed),
    "method" => method, "target" => target, "request_headers" => req_headers, "request_body_b64" => Fogfed::B64.encode(req_body),
    "answer_sealed_b64" => Fogfed::B64.encode(Fogfed::Seal.seal(req_pk, ans_info, ans_plain, ikm_e: seed(eph_ans_byte))),
    "status" => status, "answer_headers" => ans_headers, "answer_body_b64" => Fogfed::B64.encode(ans_body)
  }
end

# A sealed body to a recipient whose key is derived from a seed, with the
# ephemeral key derived from another: fixed bytes Go opens with crypto/hpke.
def seal_case(name, recipient_byte, ephemeral_byte, info, plaintext)
  sk, pk = Fogfed::Seal.derive_key_pair(seed(recipient_byte))
  {
    "name" => name, "recipient_seed_b64" => Fogfed::B64.encode(seed(recipient_byte)),
    "recipient_public_b64" => Fogfed::B64.encode(pk), "recipient_private_b64" => Fogfed::B64.encode(sk),
    "ephemeral_seed_b64" => Fogfed::B64.encode(seed(ephemeral_byte)),
    "info" => info, "plaintext_b64" => Fogfed::B64.encode(plaintext),
    "sealed_b64" => Fogfed::B64.encode(Fogfed::Seal.seal(pk, info, plaintext, ikm_e: seed(ephemeral_byte)))
  }
end

def binding_case(name, byte, instance_id, enc_seed_byte)
  id = Fogfed::Identity.from_seed(instance_id: instance_id, seed: seed(byte))
  _sk, pk = Fogfed::Seal.derive_key_pair(seed(enc_seed_byte))
  {
    "name" => name, "seed_b64" => id.seed_b64, "public_key_b64" => id.public_key_b64, "instance_id" => instance_id,
    "encryption_key_b64" => Fogfed::B64.encode(pk), "binding_b64url" => id.bind_encryption_key(pk)
  }
end

# Spellings that decode, leniently, to the same bytes as a canonical one:
# the spare bits of the last character set. Both sides must refuse them —
# a replay record keyed on text would take each for a new request.
URLSAFE = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
STD = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

def noncanonical(alphabet, canonical, spare_bits)
  last = canonical.delete("=")[-1]
  v = alphabet.index(last)
  (1...(1 << spare_bits)).map do |extra|
    c = alphabet[v | extra]
    canonical.sub(/#{Regexp.escape(last)}(=*)\z/) { c + Regexp.last_match(1) }
  end
end

def impact_case(name, subject, scope, sees, hidden, floor, addendum)
  {
    "name" => name, "subject" => subject, "scope" => scope, "sees" => sees,
    "hidden" => hidden, "floor" => floor, "addendum" => addendum,
    "sentence" => Fogfed::Impact.sentence(subject: subject, scope: scope, sees: sees,
      hidden: hidden, floor: floor, addendum: addendum)
  }
end

vectors = {
  "note" => "fogfed1 known-answer vectors. Ed25519 is deterministic, so these are fixed. " \
            "Both clients/fogfed (rake test) and fogd internal/fed (go test -run Conformance) verify them.",
  "signatures" => [
    sig_case("get no body", 0x01, "GET", "/fed/v1/files/catalog", "2026-01-02T03:04:05Z", ""),
    sig_case("post json body", 0x02, "POST", "/fed/v1/heartbeat", "2026-01-02T03:04:05Z", '{"hello":"dave"}'),
    sig_case("delete with query target", 0x03, "DELETE",
             "/fed/v1/files/shares/1/contributions?path=a.jpg", "2026-01-02T03:04:05Z", ""),
    sig_case("response pseudo-method", 0x04, "RESPONSE", "/fed/v1/heartbeat", "2026-01-02T03:04:05Z", '{"ok":true}'),
    sig_case("escaped path segment", 0x05, "GET",
             "/fed/v1/files/shares/1/blob/my%20file.jpg", "2026-01-02T03:04:05Z", ""),
    sig_case("sealed request naming its recipient", 0x0d, "POST",
             Fogfed::Envelope.sealed_target("JFRN4ZDPAVYA1F3FKPDKXKKSTG"), "2026-01-02T03:04:05.123456789Z", "sealed bytes")
  ],
  # Answers bound to the request they answer: a 2xx, a 304 (its body is the
  # entity tag), a refusal, and a streamed file by its digest. The request
  # stamps carry nanoseconds, as fogd signs them.
  "answers" => [
    answer_case("200 json", 0x07, "/fed/v1/heartbeat", "2026-01-02T03:04:05.123456789Z", 200,
                "01JREQUESTER0000000000000A", "c2lnLW9mLXRoZS1yZXF1ZXN0", '{"ok":true}'),
    answer_case("304 entity tag", 0x08, "/fed/v1/files/shares/1/manifest", "2026-01-02T03:04:05Z", 304,
                "01JREQUESTER0000000000000A", "YW5vdGhlci1yZXF1ZXN0", '"e1"'),
    answer_case("403 refusal", 0x09, "/fed/v1/flare/envelope", "2026-01-02T03:04:05Z", 403,
                "01JREQUESTER0000000000000A", "cmVmdXNlZA", '{"code":"unknown_peer","error":"unknown peer"}'),
    answer_case("refusal to an unsigned request", 0x0a, "/fed/v1/heartbeat", "2026-01-02T03:04:05Z", 403,
                "", "", '{"code":"missing_instance","error":"missing Fog-Instance"}')
  ],
  "envelopes" => [
    envelope_case("a manifest asked for, not modified", 0x20, 0x21, 0x22, 0x23,
                  "GET", "/fed/v1/files/shares/7/manifest", { "If-None-Match" => ['"e1"'] }, "",
                  304, { "Etag" => ['"e1"'] }, ""),
    envelope_case("a blob by its path", 0x24, 0x25, 0x26, 0x27,
                  "GET", "/fed/v1/files/shares/7/blob/Diary/2026/june.txt", nil, "",
                  200, { "Cache-Control" => ["private, no-store"] }, "dear diary"),
    envelope_case("a refusal", 0x28, 0x29, 0x2a, 0x2b,
                  "POST", "/fed/v1/flare/envelope", { "Content-Type" => ["application/x-sentry-envelope"] }, "{}",
                  403, nil, '{"error":"not granted","code":"forbidden"}')
  ],
  # Sealed bodies: an empty one, a request, an answer, and one long enough
  # to span records.
  "seals" => [
    seal_case("empty answer", 0x10, 0x11,
              Fogfed::Seal.answer_info("POST", "/fed/v1/sealed", "01JREQ", "c2ln", "01JRESP"), ""),
    seal_case("request", 0x12, 0x13,
              Fogfed::Seal.request_info("POST", "/fed/v1/sealed", "01JREQ", "01JRESP", "2026-01-02T03:04:05.123456789Z"),
              '{"fogVersion":"0.14.23","grants":[]}'),
    seal_case("answer", 0x14, 0x15,
              Fogfed::Seal.answer_info("POST", "/fed/v1/sealed", "01JREQ", "c2lnMg", "01JRESP"),
              '{"shares":[]}'),
    seal_case("three records", 0x16, 0x17,
              Fogfed::Seal.answer_info("POST", "/fed/v1/sealed", "01JREQ", "c2lnMw", "01JRESP"),
              (0...(2 * Fogfed::Seal::CHUNK + 1000)).map { |i| (i % 251).chr }.join.b)
  ],
  "encryption_bindings" => [
    binding_case("instance binds its key", 0x18, "01JCONFORMANCEVECTOR000001", 0x19)
  ],
  # Outer targets naming their recipient, which the request signature covers.
  "sealed_targets" => ["JFRN4ZDPAVYA1F3FKPDKXKKSTG", "pair:G6QHW3fJ4/s+zeFc2vUiHzwQNz5iP3sOHvdjZrCvcTc="].map { |r|
    { "recipient" => r, "target" => Fogfed::Envelope.sealed_target(r) }
  },
  # Instance IDs derived from identity keys, as both sides mint and check them.
  "instance_ids" => [0x40, 0x41, 0x42, 0xfe].map { |byte|
    id = Fogfed::Identity.from_seed(instance_id: "x", seed: seed(byte))
    raw = Fogfed::B64.decode(id.public_key_b64)
    { "public_key_b64" => id.public_key_b64, "instance_id" => Fogfed::Identity.derive_instance_id(raw) }
  },
  "noncanonical" => begin
    id = Fogfed::Identity.from_seed(instance_id: "01JCONFORMANCEVECTOR000001", seed: seed(0x30))
    sig = id.sign("POST", "/fed/v1/sealed", "2026-01-02T03:04:05Z", "body")
    {
      "public_key_b64" => id.public_key_b64, "timestamp" => "2026-01-02T03:04:05Z", "target" => "/fed/v1/sealed", "body" => "body",
      "signature_b64url" => sig,
      # 64 bytes in 86 characters: 4 spare bits in the last.
      "signatures" => noncanonical(URLSAFE, sig, 4),
      # 32 bytes in 44 characters with one "=": 2 spare bits.
      "public_keys" => noncanonical(STD, id.public_key_b64, 2)
    }
  end,
  "transcripts" => [
    transcript_case("fogfed1-pair", 0x06, "fogfed1-pair", "s3cr3t-invite",
                    "instanceId" => "01JCONFORMANCEVECTOR000001", "name" => "dave",
                    "publicKey" => "x").tap { |c| c },
    transcript_case("fogfed1-relay", 0x0c, "fogfed1-relay", "2026-01-02T03:09:05Z",
                    "origin" => "01JORIGIN", "author" => "01JAUTHOR", "holder" => "01JHOLDER",
                    "share" => 4, "path" => "x.txt", "exp" => "2026-01-02T03:09:05Z")
  ],
  "impacts" => [
    impact_case("all, one detail", "dave", "all 42 events of Family", ["titles", "times"], [], nil, nil),
    impact_case("with hidden and floor", "dave", "all 42 events of Family", ["titles", "times"],
                ["locations", "attendee lists"], "attendee data", "They can also comment back"),
    impact_case("picked with since", "erin", "3 hand-picked entries added since 1 January 2026",
                ["the title", "a short excerpt"], ["photos"], "private entries and photos", nil)
  ],
  # The shared lens vocabulary — every axis term, so fog (internal/fed), each
  # app, and this gem cannot drift on what "picked" or "contribute" mean.
  "lens" => Fogfed::Lens::AXES.transform_values { |terms|
    terms.map { |t| { "value" => t.value, "label" => t.label } }
  }
}

# What fogd's parsers take and refuse, exactly: Go's time.Parse(time.RFC3339)
# for a signature's timestamp, and encoding/json into the envelope's heads
# (DecodeInnerRequest plus http.NewRequest's method check; readAnswerHeadAt).
# Heads are bytes, base64-standard, so encodings travel too.
head = ->(name, text) { { "name" => name, "head_b64" => Fogfed::B64.encode(text.b) } }
vectors["timestamps"] = {
  "accept" => [
    "2026-01-02T03:04:05Z", "2026-01-02T03:04:05.123456789Z", "2026-01-02T03:04:05.1234567891Z",
    "2026-01-02T03:04:05,5Z", "2026-01-02T3:04:05Z", "2026-01-02T03:04:05+01:00", "2026-01-02T03:04:05-00:00",
    "2026-01-02T03:04:05+24:00", "2026-01-02T03:04:05-24:59", "2026-01-02T03:04:05+01:60",
    "2026-01-02T03:04:05-24:60", "2024-02-29T00:00:00Z", "2000-02-29T00:00:00Z",
    "0000-02-29T00:00:00Z", "2026-01-02T23:59:59.999Z"
  ],
  "reject" => [
    "2026-01-02T24:00:00Z", "2026-01-02T23:59:60Z", "2026-02-30T00:00:00Z", "2026-02-29T00:00:00Z",
    "1900-02-29T00:00:00Z", "2026-13-01T00:00:00Z", "2026-00-01T00:00:00Z", "2026-01-00T00:00:00Z",
    "2026-04-31T00:00:00Z", "2026-01-02T03:60:00Z", "2026-01-02T03:04:05+25:00", "2026-01-02T03:04:05+01:61",
    "2026-01-02t03:04:05Z", "2026-01-02T03:04:05z", "2026-01-02 03:04:05Z", "2026-01-02T03:04:05",
    "2026-01-02T03:04:05.Z", "2026-01-02T03:04:05,Z", "2026-01-02T03:04:05+0100", "2026-01-02T03:04:05+1:00",
    "2026-01-02T03:04:05Z ", "+2026-01-02T03:04:05Z", "2026-1-02T03:04:05Z", "2026-01-02T003:04:05Z",
    "2026-01-02T 3:04:05Z", "\uFF12026-01-02T03:04:05Z", ""
  ]
}
vectors["envelope_rejections"] = {
  "requests" => [
    head.("an array", "[1]"), head.("a string", '"GET"'), head.("a number", "1"), head.("null", "null"),
    head.("not JSON", "{"), head.("no method", '{"target":"/x"}'), head.("empty method", '{"method":"","target":"/x"}'),
    head.("method not a string", '{"method":5,"target":"/x"}'), head.("method with a space", '{"method":"GE T","target":"/x"}'),
    head.("method with a newline", '{"method":"GET\\n","target":"/x"}'), head.("method not ASCII", '{"method":"G\u00c9T","target":"/x"}'),
    head.("target not absolute", '{"method":"GET","target":"x"}'), head.("no target", '{"method":"GET"}'),
    head.("target not a string", '{"method":"GET","target":["/x"]}'),
    head.("headers an array", '{"method":"GET","target":"/x","headers":[]}'),
    head.("headers a string", '{"method":"GET","target":"/x","headers":"a"}'),
    head.("a header not a list", '{"method":"GET","target":"/x","headers":{"A":"v"}}'),
    head.("a header value a number", '{"method":"GET","target":"/x","headers":{"A":[1]}}'),
    head.("a header value an object", '{"method":"GET","target":"/x","headers":{"A":[{}]}}')
  ],
  "answers" => [
    head.("an array", "[1]"), head.("null", "null"), head.("not JSON", "{"), head.("no status", '{"digest":"d"}'),
    head.("status 99", '{"status":99}'), head.("status 600", '{"status":600}'), head.("status a string", '{"status":"200"}'),
    head.("status a float", '{"status":200.0}'), head.("status an exponent", '{"status":2e2}'),
    head.("status huge", '{"status":18446744073709551616}'), head.("status null", '{"status":null}'),
    head.("digest a number", '{"status":200,"digest":5}'), head.("ts a number", '{"status":200,"ts":5}'),
    head.("instance an object", '{"status":200,"instance":{}}'),
    head.("headers an array", '{"status":200,"headers":[]}'), head.("a header not a list", '{"status":200,"headers":{"A":"v"}}'),
    head.("a header value a bool", '{"status":200,"headers":{"A":[true]}}')
  ]
}
# And what they take that a careless parser would not: names in any case,
# null as a zero value, invalid UTF-8 as U+FFFD.
vectors["envelope_accepts"] = {
  "requests" => [
    head.("names in any case", '{"METHOD":"PATCH","Target":"/x?y=1"}').merge("method" => "PATCH", "target" => "/x?y=1", "headers" => {}),
    head.("a token method", '{"method":"M-SEARCH","target":"/"}').merge("method" => "M-SEARCH", "target" => "/", "headers" => {}),
    head.("null header values", '{"method":"GET","target":"/x","headers":{"A":[null,"b"],"B":null}}')
      .merge("method" => "GET", "target" => "/x", "headers" => { "A" => ["", "b"], "B" => nil }),
    head.("invalid UTF-8 in a value", %({"method":"GET","target":"/x","headers":{"A":["a\xffb"]}}))
      .merge("method" => "GET", "target" => "/x", "headers" => { "A" => ["a\uFFFDb"] })
  ],
  "answers" => [
    head.("names in any case", '{"STATUS":204,"Digest":"d"}').merge("status" => 204),
    head.("bounds", '{"status":100}').merge("status" => 100), head.("upper bound", '{"status":599}').merge("status" => 599),
    head.("null strings", '{"status":200,"digest":null,"ts":null,"headers":null}').merge("status" => 200)
  ]
}

File.write(File.join(__dir__, "vectors.json"), JSON.pretty_generate(vectors) + "\n")
puts "wrote #{vectors['signatures'].length} signature + #{vectors['answers'].length} answer + " \
     "#{vectors['envelopes'].length} envelope + " \
     "#{vectors['transcripts'].length} transcript " \
     "+ #{vectors['lens'].length} lens-axis vectors"
