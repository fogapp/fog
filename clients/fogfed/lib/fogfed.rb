# frozen_string_literal: true

require "openssl"

require_relative "fogfed/version"
require_relative "fogfed/b64"
require_relative "fogfed/ulid"
require_relative "fogfed/signature"
require_relative "fogfed/identity"
require_relative "fogfed/seal"
require_relative "fogfed/envelope"
require_relative "fogfed/impact"
require_relative "fogfed/lens"

# fogfed is the shared, zero-dependency implementation of Fog's federation
# identity and signing wire (fogfed1): Ed25519 identity, ULID instance ids,
# per-request signatures over the full request target, and the pairing
# transcript. It is the one place the fog-to-fog wire is defined for Ruby
# apps; fogd's Go internal/fed is its mirror, and clients/fogfed/conformance
# holds the vectors that prove the two agree.
module Fogfed
  module_function

  # Parse a wire public key (32 raw bytes, base64-standard) into a verify key.
  # Raises ArgumentError for anything else.
  def parse_public_key(b64)
    raw = B64.decode(b64)
    raise ArgumentError, "ed25519 public key must be 32 bytes" unless raw.bytesize == 32

    OpenSSL::PKey.new_raw_public_key("ED25519", raw)
  end

  # Whether instance_id may be presented with the identity key pub_b64: a
  # derived ID must be that key's own; a legacy ULID can't be checked and is
  # taken as given; anything else is not an instance ID.
  def check_instance_id(instance_id, pub_b64)
    raw = B64.decode(pub_b64)
    return false unless raw.bytesize == 32
    return false unless instance_id.is_a?(String) && instance_id.length == 26 &&
                        instance_id.delete(Identity::CROCKFORD).empty?

    case instance_id[0]
    when /[GHJKMNPQ]/ then instance_id == Identity.derive_instance_id(raw)
    when /[0-7]/ then true
    else false
    end
  rescue ArgumentError
    false
  end

  # Whether the holder of identity key pub (an Ed25519 key, as
  # parse_public_key makes) bound the encryption key enc_b64 (32 raw bytes,
  # base64-standard) as instance instance_id with signature sig.
  def verify_encryption_binding(pub, instance_id, enc_b64, sig)
    raw = B64.decode(enc_b64)
    return false unless raw.bytesize == 32

    Signature.verify_transcript(pub, "fogfed1-enckey", instance_id, raw, sig)
  rescue ArgumentError
    false
  end
end
