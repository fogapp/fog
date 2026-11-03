# frozen_string_literal: true

require_relative "lib/flare/version"

Gem::Specification.new do |s|
  s.name        = "flare"
  s.version     = Flare::VERSION
  s.summary     = "Error reporting to Fog's built-in tracker"
  s.description = "Zero-dependency Rails error reporter. Subscribes to Rails.error and ships " \
                  "Sentry-schema events to the FLARE_DSN Fog injects into every managed app."
  s.authors     = ["Matthew Werner"]
  s.homepage    = "https://gitea.lan/matthew/fog"
  s.license     = "Nonstandard"
  s.files       = Dir["lib/**/*.rb", "README.md"]
  s.required_ruby_version = ">= 3.2"
  # No runtime dependencies, ever — this gem rides inside every app and must
  # not be able to conflict with anything they use. Development-only deps are
  # fine: they never install with the vendored gem.
  s.add_development_dependency "minitest", "~> 5.16"
  s.add_development_dependency "rake", "~> 13.0"
end
