# frozen_string_literal: true

module Fogfed
  # The two base64 forms the fogfed1 wire uses, without the base64 gem: it
  # left Ruby's default gems in 3.4, so under Bundler `require "base64"` fails
  # unless the app's Gemfile names it, and this gem has no dependencies.
  # pack("m0") is RFC 4648 strict, and decoding is as strict as fogd's Go:
  # keys and seeds are padded standard base64 (base64.StdEncoding),
  # signatures unpadded base64url (base64.RawURLEncoding), and anything else
  # (padding on a signature, the other alphabet, stray bytes) is an
  # ArgumentError.
  module B64
    module_function

    def encode(bytes) = [bytes.to_s].pack("m0")

    def decode(str)
      s = str.to_s
      raise ArgumentError, "invalid base64" unless s.match?(%r{\A(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?\z})
      s.unpack1("m0")
    end

    def urlsafe_encode(bytes) = encode(bytes).tr("+/", "-_").delete("=")

    def urlsafe_decode(str)
      s = str.to_s
      raise ArgumentError, "invalid base64url" unless s.match?(/\A[A-Za-z0-9_-]*\z/) && s.length % 4 != 1
      decode(s.tr("-_", "+/") + ("=" * ((4 - (s.length % 4)) % 4)))
    end
  end
end
