# frozen_string_literal: true

require "openssl"
require "digest"
require "time"
require_relative "b64"

module Fogfed
  # The fogfed1 wire discipline: every fed-plane request after pairing is
  # authenticated by an Ed25519 signature over the version tag, the method,
  # the full request target (path AND query), the timestamp, and the body
  # digest. Every answer signs under the pseudo-method "ANSWER" over
  # material that binds it to the request it answers — the requester's
  # Fog-Instance and Fog-Signature, and the status — so a captured answer
  # verifies for no other exchange. In fogd's envelope (Fogfed::Envelope) what
  # an answer signs is its head. This
  # is a byte-for-byte port of fogd's internal/fed/sign.go — the conformance
  # vectors prove it.
  #
  # "RESPONSE" (an answer signed over its body alone) is the older, unbound
  # form. fogd neither sends nor accepts it; it is kept only so an app's own
  # wire built on it keeps working while it moves to sign_answer.
  module Signature
    SIG_VERSION = "fogfed1"
    RESPONSE_METHOD = "RESPONSE" # unbound; see sign_answer
    ANSWER_METHOD = "ANSWER"
    STAMP_MUTEX = Mutex.new
    MAX_SKEW = 300 # seconds; fogd's ±5-minute window

    HEADER_INSTANCE      = "Fog-Instance"
    HEADER_TIMESTAMP     = "Fog-Timestamp"
    HEADER_SIGNATURE     = "Fog-Signature"
    HEADER_CONTENT_SHA256 = "Fog-Content-SHA256"

    # RFC 3339 exactly as Go's time.Parse(time.RFC3339) reads it, which is
    # what fogd checks: a four-digit year; two-digit month and day, the day
    # real for its month (year 0 is a leap year); an hour of one or two
    # digits below 24; two-digit minutes and seconds below 60 (no leap
    # second); a fraction after "." or "," of any length; then "Z" or a
    # +hh:mm offset whose hours go to 24 and minutes to 60. Nothing else,
    # nothing after. Time.iso8601 takes 24:00:00, 23:59:60 and 2026-02-30,
    # which Go refuses; conformance/vectors.json "timestamps" pins the two.
    RFC3339 = /\A(\d{4})-(\d{2})-(\d{2})T(\d{1,2}):(\d{2}):(\d{2})(?:[.,](\d+))?(?:Z|([+-])(\d{2}):(\d{2}))\z/
    DAYS_IN_MONTH = [nil, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31].freeze


    class SkewError < StandardError; end
    class BadSignature < StandardError; end
    # A request whose signature verify! has already accepted (seen:).
    class Replayed < BadSignature; end

    # An in-process seen: for verify!: it remembers each accepted signature
    # until its timestamp would fail the skew check anyway, then forgets it.
    # Enough for one process; an app with several (Puma workers, replicas)
    # needs a shared store instead (README, "Replay"). Full (max live
    # signatures inside the window), it refuses rather than forget one that
    # could still be replayed.
    class ReplayCache
      def initialize(max: 100_000)
        @max = max
        @seen = {}
        @calls = 0
        @mutex = Mutex.new
      end

      # true when key was seen before (a replay); records it otherwise.
      def call(key, expires_at, now: Time.now)
        @mutex.synchronize do
          @calls += 1
          @seen.delete_if { |_, exp| exp < now } if @seen.size >= @max || (@calls % 1000).zero?
          return true if @seen.key?(key)
          return true if @seen.size >= @max

          @seen[key] = expires_at
          false
        end
      end
    end

    module_function

    # The instant timestamp names, or ArgumentError where Go's parse fails.
    def parse_timestamp(timestamp)
      m = timestamp.is_a?(String) && RFC3339.match(timestamp)
      raise ArgumentError, "not an RFC 3339 timestamp" unless m

      year, month, day, hour, min, sec = m[1..6].map { |x| Integer(x, 10) }
      leap = (year % 4).zero? && (!(year % 100).zero? || (year % 400).zero?)
      days = month == 2 && leap ? 29 : DAYS_IN_MONTH[month] if (1..12).cover?(month)
      raise ArgumentError, "month out of range" unless days
      raise ArgumentError, "day out of range" unless (1..days).cover?(day)
      raise ArgumentError, "hour out of range" unless hour < 24
      raise ArgumentError, "minute out of range" unless min < 60
      raise ArgumentError, "second out of range" unless sec < 60

      offset = 0
      if m[8]
        oh, om = Integer(m[9], 10), Integer(m[10], 10)
        raise ArgumentError, "time zone offset hour out of range" if oh > 24
        raise ArgumentError, "time zone offset minute out of range" if om > 60
        offset = (m[8] == "-" ? -1 : 1) * ((oh * 3600) + (om * 60))
      end
      frac = m[7] ? Rational(Integer(m[7], 10), 10**m[7].length) : 0
      Time.utc(year, month, day, hour, min, sec) + frac - offset
    end

    # The canonical bytes a fogfed1 signature covers.
    def signing_string(method, target, timestamp, body)
      "#{SIG_VERSION}\n#{method}\n#{target}\n#{timestamp}\n#{Digest::SHA256.hexdigest(body.to_s)}"
    end

    # Sign returns the base64url (no padding) signature value.
    def sign(private_key, method, target, timestamp, body = "")
      raw = ed25519!(private_key).sign(nil, signing_string(method, target, timestamp, body))
      B64.urlsafe_encode(raw)
    end

    # RFC3339 UTC with nanoseconds, strictly later than any this process
    # stamped before — fogd's uniqueStamp — so no two requests one signer
    # makes are the same bytes, and a receiver can refuse any signature it
    # has already seen without ever refusing a genuine request.
    def now_timestamp(now = nil)
      return stamp(now) if now # a stated time is stamped as given

      ns = (Time.now.to_r * 1_000_000_000).to_i
      STAMP_MUTEX.synchronize do
        ns = @last_stamp + 1 if @last_stamp && ns <= @last_stamp
        @last_stamp = ns
      end
      stamp(Rational(ns, 1_000_000_000))
    end

    # RFC 3339 UTC, with as many fractional digits as the time carries.
    def stamp(time)
      ns = (time.to_r * 1_000_000_000).to_i
      t = Time.at(Rational(ns, 1_000_000_000)).utc
      frac = format("%09d", ns % 1_000_000_000).sub(/0+\z/, "")
      frac.empty? ? t.strftime("%Y-%m-%dT%H:%M:%SZ") : t.strftime("%Y-%m-%dT%H:%M:%S.") + frac + "Z"
    end

    # What an ANSWER signs in place of the bare body: the status and the
    # request answered (its Fog-Instance and Fog-Signature), then the body.
    # A 304's body is the entity tag it confirms.
    def answer_material(status, request_instance, request_signature, body)
      "#{Integer(status)}\n#{request_instance}\n#{request_signature}\n#{body}"
    end

    def sign_answer(private_key, target, timestamp, status, request_instance, request_signature, body = "")
      sign(private_key, ANSWER_METHOD, target, timestamp, answer_material(status, request_instance, request_signature, body))
    end

    def verify_answer!(public_key, target, timestamp, signature_b64url, status, request_instance, request_signature, body = "", now: Time.now)
      verify!(public_key, ANSWER_METHOD, target, timestamp, signature_b64url,
              answer_material(status, request_instance, request_signature, body), now: now)
    end


    # Verify a request or response signature and its timestamp skew. Returns
    # true, or raises SkewError / BadSignature. public_key must be Ed25519
    # (Fogfed.parse_public_key makes one): any other key type is an
    # ArgumentError, never a verification under another algorithm.
    #
    # A valid signature verifies again for as long as its timestamp is in the
    # window (MAX_SKEW either side), so a captured request can be sent twice.
    # seen: closes that: anything answering call(key, expires_at) with true
    # when key was accepted before (and recording it otherwise), such as a
    # ReplayCache, or a shared store for an app with several processes. It
    # is asked only once the signature verifies, so forgeries can't fill it;
    # key is the signature, and expires_at is when the timestamp leaves the
    # window. A replay raises Replayed, a BadSignature.
    def verify!(public_key, method, target, timestamp, signature_b64url, body = "", now: Time.now, seen: nil)
      ed25519!(public_key)
      sent = begin
        parse_timestamp(timestamp)
      rescue ArgumentError
        raise BadSignature, "invalid federation timestamp"
      end
      raise SkewError, "federation timestamp outside the allowed window" if (now - sent).abs > MAX_SKEW

      sig = begin
        B64.urlsafe_decode(signature_b64url)
      rescue ArgumentError
        raise BadSignature, "malformed federation signature"
      end
      unless public_key.verify(nil, sig, signing_string(method, target, timestamp, body))
        raise BadSignature, "federation signature does not verify"
      end
      if seen && seen.call(B64.urlsafe_encode(sig), sent + MAX_SKEW)
        raise Replayed, "federation request already seen"
      end
      true
    end

    # verify! as a boolean. A key that isn't Ed25519 still raises.
    def verify(*args, **kw)
      verify!(*args, **kw)
    rescue SkewError, BadSignature
      false
    end

    # The pairing/handshake transcript: a labelled construction over a nonce
    # (an invite secret, or an issued-at) and the sha256 of an instance
    # document. Shared with fogd's PairSign and hub's hubfed1-* labels — the
    # label carries the app and step ("fogfed1-pair", "hubfed1-handshake").
    def transcript(label, nonce, doc_json)
      "#{label}\n#{nonce}\n#{Digest::SHA256.hexdigest(doc_json.to_s)}"
    end

    def sign_transcript(private_key, label, nonce, doc_json)
      B64.urlsafe_encode(ed25519!(private_key).sign(nil, transcript(label, nonce, doc_json)))
    end

    # True or false. A key that isn't Ed25519 raises, as in verify!.
    def verify_transcript(public_key, label, nonce, doc_json, signature_b64url)
      ed25519!(public_key)
      sig = begin
        B64.urlsafe_decode(signature_b64url)
      rescue ArgumentError
        return false
      end
      public_key.verify(nil, sig, transcript(label, nonce, doc_json))
    end

    # The fogfed1 wire is Ed25519 and nothing else: refuse any other key
    # rather than let OpenSSL sign or verify under its algorithm.
    def ed25519!(key)
      unless key.is_a?(OpenSSL::PKey::PKey) && key.oid == "ED25519"
        raise ArgumentError, "fogfed1 keys are Ed25519, not #{key.class}"
      end
      key
    end
  end
end
