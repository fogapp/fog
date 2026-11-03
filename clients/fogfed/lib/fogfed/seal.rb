# frozen_string_literal: true

require "openssl"
require "digest"
require_relative "b64"

module Fogfed
  # Sealed bodies, as fogd seals them (internal/fed/seal.go): HPKE
  # (RFC 9180) base mode, suite DHKEM(X25519, HKDF-SHA256) / HKDF-SHA256 /
  # ChaCha20-Poly1305, one HPKE context per body, the body framed as records
  # so a stream of any size opens with bounded memory:
  #
  #   0x01 || enc (32 bytes) || record*
  #   record = len (uint32 BE) || Seal(aad = final flag byte, chunk)
  #
  # The final record carries aad 0x01, every other 0x00; a stream cut short,
  # reordered, spliced or carrying anything after its final record does not
  # open. The context's info binds the body to its exchange (request_info,
  # answer_info). Built from OpenSSL's X25519, HMAC-SHA256 and
  # ChaCha20-Poly1305 following the RFC step for step; the conformance
  # vectors are opened by Go's crypto/hpke, byte for byte.
  module Seal
    class Error < StandardError; end

    KEM_ID = 0x0020
    KDF_ID = 0x0001
    AEAD_ID = 0x0003
    N_SECRET = 32
    N_K = 32
    N_N = 12
    N_T = 16
    VERSION = "\x01".b
    ENC_LEN = 32
    CHUNK = 64 * 1024
    MAX_RECORD = CHUNK + N_T

    KEM_SUITE = "KEM".b + [KEM_ID].pack("n")
    HPKE_SUITE = "HPKE".b + [KEM_ID, KDF_ID, AEAD_ID].pack("nnn")

    module_function

    # ---- info: what a body's sealing binds ----

    def request_info(method, target, requester, recipient, timestamp)
      "fogfed1-request\n#{method}\n#{target}\n#{requester}\n#{recipient}\n#{timestamp}".b
    end

    def answer_info(method, target, requester, request_signature, responder)
      "fogfed1-answer\n#{method}\n#{target}\n#{requester}\n#{request_signature}\n#{responder}".b
    end

    # ---- keys ----

    # DeriveKeyPair (RFC 9180 7.1.3) from a 32-byte seed: [sk, pk] raw.
    def derive_key_pair(ikm)
      dkp_prk = labeled_extract("".b, "dkp_prk", ikm.b, KEM_SUITE)
      sk = labeled_expand(dkp_prk, "sk", "".b, N_SECRET, KEM_SUITE)
      [sk, public_of(sk)]
    end

    def public_of(sk)
      OpenSSL::PKey.new_raw_private_key("X25519", sk).raw_public_key
    end

    # ---- sealing ----

    # Seal plaintext to pk_r (raw 32 bytes) for info. ikm_e, when given, is
    # the seed the ephemeral key is derived from — for known-answer vectors
    # only; leave it nil to draw a fresh one.
    def seal(pk_r, info, plaintext, ikm_e: nil)
      sender = Sender.new(pk_r, info, ikm_e: ikm_e)
      out = VERSION + sender.enc
      data = plaintext.to_s.b
      chunks = (0...[data.bytesize, 1].max).step(CHUNK).map { |i| data.byteslice(i, CHUNK) || "".b }
      last = chunks.length - 1
      chunks.each_with_index do |chunk, i|
        ct = sender.seal((i == last ? "\x01" : "\x00").b, chunk)
        out << [ct.bytesize].pack("N") << ct
      end
      out
    end

    # Open a sealed body sent to sk_r (raw 32 bytes) for info. Raises
    # Seal::Error for anything that isn't exactly what was sealed.
    def open(sk_r, info, sealed)
      s = sealed.to_s.b
      raise Error, "no sealed header" unless s.bytesize >= 1 + ENC_LEN && s.byteslice(0, 1) == VERSION

      recipient = Recipient.new(sk_r, s.byteslice(1, ENC_LEN), info)
      pos = 1 + ENC_LEN
      out = "".b
      loop do
        raise Error, "cut short" if pos + 4 > s.bytesize

        n = s.byteslice(pos, 4).unpack1("N")
        raise Error, "bad record length" if n < N_T || n > MAX_RECORD
        raise Error, "cut short" if pos + 4 + n > s.bytesize

        ct = s.byteslice(pos + 4, n)
        pos += 4 + n
        if (pt = recipient.try_open("\x00".b, ct))
          out << pt
          next
        end
        pt = recipient.try_open("\x01".b, ct)
        raise Error, "record does not authenticate" unless pt
        raise Error, "data after the final record" if pos != s.bytesize

        return out << pt
      end
    end

    # ---- RFC 9180 ----

    def labeled_extract(salt, label, ikm, suite)
      OpenSSL::HMAC.digest("SHA256", salt.empty? ? ("\x00" * 32).b : salt, "HPKE-v1".b + suite + label.b + ikm)
    end

    def labeled_expand(prk, label, info, len, suite)
      labeled_info = [len].pack("n") + "HPKE-v1".b + suite + label.b + info
      out = "".b
      t = "".b
      i = 1
      while out.bytesize < len
        t = OpenSSL::HMAC.digest("SHA256", prk, t + labeled_info + [i].pack("C"))
        out << t
        i += 1
      end
      out.byteslice(0, len)
    end

    def dh(sk, pk)
      ours = OpenSSL::PKey.new_raw_private_key("X25519", sk)
      theirs = OpenSSL::PKey.new_raw_public_key("X25519", pk)
      ours.derive(theirs)
    end

    def shared_secret(dh_out, kem_context)
      eae_prk = labeled_extract("".b, "eae_prk", dh_out, KEM_SUITE)
      labeled_expand(eae_prk, "shared_secret", kem_context, N_SECRET, KEM_SUITE)
    end

    # KeySchedule, mode_base: [key, base_nonce].
    def key_schedule(shared, info)
      psk_id_hash = labeled_extract("".b, "psk_id_hash", "".b, HPKE_SUITE)
      info_hash = labeled_extract("".b, "info_hash", info.b, HPKE_SUITE)
      context = "\x00".b + psk_id_hash + info_hash
      secret = labeled_extract(shared, "secret", "".b, HPKE_SUITE)
      [labeled_expand(secret, "key", context, N_K, HPKE_SUITE),
       labeled_expand(secret, "base_nonce", context, N_N, HPKE_SUITE)]
    end

    def nonce(base, seq)
      seq_bytes = ("\x00" * (N_N - 8)).b + [seq].pack("Q>")
      base.bytes.zip(seq_bytes.bytes).map { |a, b| a ^ b }.pack("C*")
    end

    def aead_seal(key, nonce, aad, pt)
      c = OpenSSL::Cipher.new("chacha20-poly1305").encrypt
      c.key = key
      c.iv = nonce
      c.auth_data = aad
      ct = pt.empty? ? "".b : c.update(pt)
      ct << c.final
      ct + c.auth_tag(N_T)
    end

    def aead_open(key, nonce, aad, ct)
      return nil if ct.bytesize < N_T

      c = OpenSSL::Cipher.new("chacha20-poly1305").decrypt
      c.key = key
      c.iv = nonce
      c.auth_tag = ct.byteslice(-N_T, N_T)
      c.auth_data = aad
      body = ct.byteslice(0, ct.bytesize - N_T)
      pt = body.empty? ? "".b : c.update(body)
      pt << c.final
      pt
    rescue OpenSSL::Cipher::CipherError
      nil
    end

    # A sending context: Encap, then a counter per record.
    class Sender
      attr_reader :enc

      def initialize(pk_r, info, ikm_e: nil)
        sk_e, pk_e = Seal.derive_key_pair(ikm_e || OpenSSL::Random.random_bytes(32))
        @enc = pk_e
        shared = Seal.shared_secret(Seal.dh(sk_e, pk_r), pk_e + pk_r)
        @key, @base = Seal.key_schedule(shared, info)
        @seq = 0
      end

      def seal(aad, pt)
        ct = Seal.aead_seal(@key, Seal.nonce(@base, @seq), aad, pt.b)
        @seq += 1
        ct
      end
    end

    # A receiving context: Decap, then a counter that moves only on a record
    # that opens.
    class Recipient
      def initialize(sk_r, enc, info)
        pk_r = Seal.public_of(sk_r)
        shared = Seal.shared_secret(Seal.dh(sk_r, enc), enc + pk_r)
        @key, @base = Seal.key_schedule(shared, info)
        @seq = 0
      rescue OpenSSL::PKey::PKeyError
        raise Error, "bad encapsulated key"
      end

      def try_open(aad, ct)
        pt = Seal.aead_open(@key, Seal.nonce(@base, @seq), aad, ct)
        @seq += 1 if pt
        pt
      end
    end
  end
end
