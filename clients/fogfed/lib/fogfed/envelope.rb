# frozen_string_literal: true

require "json"
require "digest"
require_relative "seal"
require_relative "signature"

module Fogfed
  # The envelope fogd's fed plane crosses the wire in (internal/fed/envelope.go):
  # every exchange is a POST to SEALED_PATH whose body is the real request,
  # sealed, answered by a 200 whose body is the real answer, sealed — method,
  # target, headers and status all inside. An observer sees that two fogs
  # talk, the caller's instance ID and public keys, sizes and timing.
  #
  #   request plaintext = u32 len(R) || R || body
  #     R = {"method", "target", "headers"}
  #   answer plaintext  = u32 len(H) || H || u16 len(S) || S || body
  #     H = {"status", "headers", "digest", "instance", "ts", ...}
  #     S = Signature.sign_answer(key, target, H.ts, status, request instance, request signature, H)
  #
  # The answer's signature covers its head as sent, bound to the request;
  # the head's digest (sha256hex) covers the body.
  module Envelope
    SEALED_PATH = "/fed/v1/sealed"
    MAX_HEAD = 64 * 1024

    class Error < StandardError; end

    # An HTTP token (RFC 9110 tchar), the methods Go's http.NewRequest takes.
    METHOD = /\A[!#$%&'*+\-.^_`|~0-9A-Za-z]+\z/

    module_function

    # The outer target of a request to recipient (an instance ID, or a pair
    # recipient "pair:<key>"): SEALED_PATH naming it in ?to=, escaped as Go's
    # url.QueryEscape escapes it. The signature covers the target, so signed
    # headers are good only at the fog they were made for.
    def sealed_target(recipient)
      SEALED_PATH + "?to=" + recipient.to_s.b.gsub(/[^A-Za-z0-9\-_.~]/n) { |c| format("%%%02X", c.ord) }
    end

    # A head read as Go's encoding/json reads it into fogd's struct, so the
    # two sides accept and refuse the same bytes. fields maps each field's
    # JSON name to its Go type (:string, :int or :header, http.Header, a
    # map[string][]string). As in Go: names match case-insensitively; null
    # leaves a field its zero value ("", 0, nil) and a null header value or
    # element its zero value; invalid UTF-8 reads as U+FFFD per byte; a JSON
    # null head is all zero values; any other non-object, or a value of the
    # wrong type (a float for an int, a string for a header list), is
    # refused. One deliberate difference: Go lets the last of two keys for
    # one field win, and this refuses a head with two (a json gem limit, and
    # no honest encoder writes one). Returns { name => value } for every
    # field, zero values included.
    def go_struct(bytes, fields)
      text = bytes.to_s.dup.force_encoding(Encoding::UTF_8).scrub { |b| "\uFFFD" * b.bytesize }
      obj = JSON.parse(text, allow_duplicate_key: false)
      zero = { string: "", int: 0, header: nil }
      return fields.to_h { |name, type| [name, zero[type]] } if obj.nil?
      raise Error, "not an object" unless obj.is_a?(Hash)

      fields.to_h do |name, type|
        keys = obj.keys.select { |k| k.downcase(:fold) == name.downcase(:fold) }
        raise Error, "two keys for #{name}" if keys.length > 1

        [name, go_value(keys.empty? ? nil : obj[keys.first], type, zero[type])]
      end
    rescue JSON::ParserError, EncodingError
      raise Error, "not JSON"
    end

    def go_value(v, type, zero)
      return zero if v.nil?

      case type
      when :string then v.is_a?(String) ? v : raise(Error, "not a string")
      when :int then v.is_a?(Integer) && v.bit_length < 64 ? v : raise(Error, "not an integer")
      when :header
        raise Error, "headers are not an object" unless v.is_a?(Hash)

        v.transform_values do |list|
          next nil if list.nil?
          raise Error, "a header is not a list" unless list.is_a?(Array)

          list.map { |e| e.nil? ? "" : (e.is_a?(String) ? e : raise(Error, "a header value is not a string")) }
        end
      end
    end

    REQUEST_FIELDS = { "method" => :string, "target" => :string, "headers" => :header }.freeze
    ANSWER_FIELDS = { "status" => :int, "headers" => :header, "digest" => :string, "instance" => :string,
                      "ts" => :string, "encryptionKey" => :string, "encryptionKeySig" => :string }.freeze

    # An answer head's bytes as fogd reads them (readAnswerHeadAt), before
    # its signature: a JSON object of the right shape whose status is 100-599.
    # => the head's fields, zero values included.
    def parse_answer_head(bytes)
      head = go_struct(bytes, ANSWER_FIELDS)
      raise Error, "bad answer head" unless (100..599).cover?(head["status"])

      head
    rescue Error
      raise Error, "bad answer head"
    end

    # headers: { "Name" => ["value", ...] } (or nil).
    def encode_request(method, target, headers, body)
      head = JSON.generate({ "method" => method, "target" => target, "headers" => headers }.compact)
      [head.bytesize].pack("N") + head.b + body.to_s.b
    end

    # => [method, target, headers, body]
    def decode_request(plain)
      p = plain.to_s.b
      raise Error, "no inner request" if p.bytesize < 4

      n = p.byteslice(0, 4).unpack1("N")
      raise Error, "bad inner request head" if n > MAX_HEAD || n > p.bytesize - 4

      r = go_struct(p.byteslice(4, n), REQUEST_FIELDS)
      # DecodeInnerRequest's checks, then http.NewRequest's on the method.
      raise Error unless METHOD.match?(r["method"]) && r["target"].start_with?("/")

      [r["method"], r["target"], r["headers"] || {}, p.byteslice(4 + n, p.bytesize - 4 - n)]
    rescue Error
      raise Error, "bad inner request head"
    end

    # An answer's plaintext: its head, signed by identity for the request
    # (request_instance, request_signature) at target, then body.
    def encode_answer(identity, target, timestamp, request_instance, request_signature, status, headers, body, extra = {})
      body = body.to_s.b
      head = JSON.generate({ "status" => status, "headers" => headers, "digest" => Digest::SHA256.hexdigest(body),
                             "instance" => identity.instance_id, "ts" => timestamp }.compact.merge(extra))
      sig = identity.sign_answer(target, timestamp, status, request_instance, request_signature, head)
      [head.bytesize].pack("N") + head.b + [sig.bytesize].pack("n") + sig.b + body
    end

    # => [head (Hash), body]. Verifies the head's signature with pub (an
    # Ed25519 key) for the request at target, and the body against the
    # head's digest. Raises Error, or Signature's errors.
    def decode_answer(plain, pub, target, request_instance, request_signature, now: Time.now)
      p = plain.to_s.b
      raise Error, "no answer head" if p.bytesize < 4

      n = p.byteslice(0, 4).unpack1("N")
      raise Error, "answer head too large" if n > MAX_HEAD || 4 + n + 2 > p.bytesize

      head_bytes = p.byteslice(4, n)
      sl = p.byteslice(4 + n, 2).unpack1("n")
      raise Error, "answer head cut short" if 4 + n + 2 + sl > p.bytesize

      sig = p.byteslice(4 + n + 2, sl)
      body = p.byteslice(4 + n + 2 + sl, p.bytesize - (4 + n + 2 + sl))
      head = parse_answer_head(head_bytes)
      Signature.verify_answer!(pub, target, head["ts"], sig, head["status"], request_instance, request_signature, head_bytes, now: now)
      raise Error, "the body does not match its signed digest" unless Digest::SHA256.hexdigest(body) == head["digest"]

      [head, body]
    end
  end
end
