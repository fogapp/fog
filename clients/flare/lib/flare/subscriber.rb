# frozen_string_literal: true

module Flare
  # The one integration point on Rails 7.1+: Rails wraps HTTP requests,
  # ActiveJob jobs, and runners in the error reporter, so subscribing here
  # covers unhandled crashes everywhere without middleware or patches.
  class Subscriber
    def report(error, handled:, severity:, context:, source: nil)
      Flare.notify(error, handled: handled, severity: severity, context: context, source: source)
    end
  end
end
