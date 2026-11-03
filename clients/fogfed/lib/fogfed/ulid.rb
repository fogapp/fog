# frozen_string_literal: true

require "securerandom"

module Fogfed
  # A 26-character ULID: 48 bits of millisecond timestamp then 80 bits of
  # randomness, Crockford base32 — byte-for-byte the layout fogd mints
  # (internal/fed/ulid.go), so a fogfed id and a fogd id are interchangeable.
  module Ulid
    CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

    module_function

    def generate(time_ms: nil)
      ms = (time_ms || (Time.now.to_f * 1000).floor).to_i
      b = Array.new(16, 0)
      5.downto(0) { |i| b[i] = ms & 0xff; ms >>= 8 }
      SecureRandom.bytes(10).bytes.each_with_index { |byte, i| b[6 + i] = byte }
      out = +""
      26.times do |i|
        start = (i * 5) - 2
        v = 0
        5.times do |j|
          bit = start + j
          v <<= 1
          v |= 1 if bit >= 0 && (b[bit / 8] & (1 << (7 - (bit % 8)))) != 0
        end
        out << CROCKFORD[v]
      end
      out
    end
  end
end
