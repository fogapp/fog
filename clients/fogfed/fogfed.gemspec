# frozen_string_literal: true

require_relative "lib/fogfed/version"

Gem::Specification.new do |s|
  s.name        = "fogfed"
  s.version     = Fogfed::VERSION
  s.summary     = "Fog's federation identity and signing wire (fogfed1)"
  s.description = "Zero-dependency Ruby implementation of the fogfed1 wire: Ed25519 " \
                  "identity, ULID instance ids, per-request signatures over the full " \
                  "request target, and the pairing transcript. The mirror of fogd's " \
                  "Go internal/fed; clients/fogfed/conformance holds the vectors that " \
                  "prove the two agree byte for byte."
  s.authors     = ["Matthew Werner"]
  s.homepage    = "https://github.com/fogapp/fog"
  s.license     = "MIT"
  s.files       = Dir["lib/**/*.rb", "conformance/*.json", "README.md", "LICENSE"]
  s.required_ruby_version = ">= 3.2"
  # No runtime dependencies, ever — this gem rides vendored inside every app
  # and must not be able to conflict with anything they use.
  s.add_development_dependency "minitest", "~> 5.16"
  s.add_development_dependency "rake", "~> 13.0"
end
