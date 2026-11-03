# frozen_string_literal: true

require "openssl"
require_relative "b64"

module Fogfed
  # An instance's federation identity: one Ed25519 keypair and the instance
  # ID derived from it, the shape fogd mints (internal/fed/identity.go). The
  # private seed is 32 bytes; the public key crosses the wire
  # base64-standard-encoded.
  class Identity
    attr_reader :instance_id

    CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

    # The instance ID that belongs to an identity key (32 raw bytes):
    # Crockford base32, 26 characters, of the marker bits 10 and the first
    # 128 bits of sha256("fogfed1-instance\n" + key). It starts with one of
    # G H J K M N P Q; a legacy, random ULID starts with 0-7.
    def self.derive_instance_id(public_key_raw)
      digest = OpenSSL::Digest::SHA256.digest("fogfed1-instance\n".b + public_key_raw.b)
      n = (2 << 128) | digest[0, 16].unpack1("H*").to_i(16)
      (0...26).map { |i| CROCKFORD[(n >> (5 * (25 - i))) & 31] }.join
    end

    # A fresh identity: new keypair, its derived instance ID.
    def self.generate
      key = OpenSSL::PKey.generate_key("ED25519")
      new(instance_id: derive_instance_id(key.raw_public_key), private_key: key)
    end

    # Rebuild from a stored 32-byte seed (raw bytes or base64-standard).
    def self.from_seed(instance_id:, seed:)
      raw = seed.to_s.bytesize == 32 ? seed : B64.decode(seed)
      new(instance_id: instance_id, private_key: OpenSSL::PKey.new_raw_private_key("ED25519", raw))
    end

    def initialize(instance_id:, private_key:)
      @instance_id = instance_id
      @private_key = private_key
    end

    # The raw 32-byte Ed25519 seed — what to store, vault-encrypted.
    def seed = @private_key.raw_private_key
    def seed_b64 = B64.encode(seed)

    def public_key = OpenSSL::PKey.new_raw_public_key("ED25519", @private_key.raw_public_key)
    def public_key_b64 = B64.encode(@private_key.raw_public_key)

    def sign(method, target, timestamp, body = "")
      Signature.sign(@private_key, method, target, timestamp, body)
    end

    # An answer to the request carrying request_instance and
    # request_signature (see Signature.sign_answer).
    def sign_answer(target, timestamp, status, request_instance, request_signature, body = "")
      Signature.sign_answer(@private_key, target, timestamp, status, request_instance, request_signature, body)
    end


    # The binding of an encryption key to this identity: the transcript
    # "fogfed1-enckey\n<instance id>\n<sha256hex(key)>", signed — what a
    # peer checks before it seals anything to that key.
    def bind_encryption_key(enc_public_raw)
      Signature.sign_transcript(@private_key, "fogfed1-enckey", instance_id, enc_public_raw)
    end

    def sign_transcript(label, nonce, doc_json)
      Signature.sign_transcript(@private_key, label, nonce, doc_json)
    end

    # The instance document exchanged at pair time:
    # {instanceId, name, publicKey, url?, fogVersion?}.
    def instance_doc(name:, url: nil, version: nil)
      doc = { "instanceId" => instance_id, "name" => name, "publicKey" => public_key_b64 }
      doc["url"] = url if url && !url.to_s.empty?
      doc["fogVersion"] = version if version && !version.to_s.empty?
      doc
    end
  end
end
