# frozen_string_literal: true

require_relative "lib/fog_client/version"

Gem::Specification.new do |s|
  s.name        = "fog-client"
  s.version     = FogClient::VERSION
  s.summary     = "The app-side view of Fog's federation"
  s.description = "Zero-dependency client for the FOG_API_URL surface Fog injects into every " \
                  "managed app: list federated peers and their exposed apps, request " \
                  "introductions. Inert without the env vars — apps must run identically " \
                  "outside Fog (docs/federation-contract.md)."
  s.authors     = ["Matthew Werner"]
  s.homepage    = "https://gitea.lan/matthew/fog"
  s.license     = "Nonstandard"
  s.files       = Dir["lib/**/*.rb", "rbi/*.rbi", "README.md"]
  s.required_ruby_version = ">= 3.2"
  # No runtime dependencies, ever — this gem rides inside every app and must
  # not be able to conflict with anything they use. Development-only deps are
  # fine: they never install with the vendored gem.
  s.add_development_dependency "minitest", "~> 5.16"
  s.add_development_dependency "rake", "~> 13.0"
end
