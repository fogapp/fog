# frozen_string_literal: true

module Flare
  class Railtie < Rails::Railtie
    # Subscribe as early as possible so initializer-time errors report too.
    # Unconditionally: Subscriber#report -> Flare.notify already no-ops when
    # no DSN is configured, so without one the app runs exactly as before —
    # and a DSN set later (Flare.configure in an app initializer) still gets
    # its errors reported instead of being silently inert.
    initializer "flare.error_subscriber" do
      Rails.error.subscribe(Flare::Subscriber.new)
    end

    config.after_initialize do
      if Flare.enabled?
        Flare.config.logger ||= Rails.logger
        # Honor the app's own PII rules on top of the built-in patterns.
        Flare.config.rails_filters = Rails.application.config.filter_parameters || []
      end
    end
  end
end
