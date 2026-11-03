# frozen_string_literal: true

module Fogfed
  # The consent grammar's shared vocabulary. A federated share is a lens, and
  # three of its axes are the *same words* wherever sharing happens — fog's
  # file shares, hub's calendars and chronicles — so a person reads the same
  # promise everywhere. Those axes live here, once, and the conformance vectors
  # prove Go (internal/fed) and Ruby agree term for term. (The fourth axis,
  # detail, is deliberately absent: it is kind-specific — files run
  # full/previews/listing, records run field-group presets — so it is not
  # shared vocabulary and each kind names its own.)
  #
  # Each term is a value object carrying the wire value and a human label; the
  # longer definitions live in docs/consent-grammar.md, the prose home.
  module Lens
    Term = Struct.new(:value, :label, keyword_init: true) do
      def to_s = value
    end

    # mode — how far the peer may act.
    MODES = [
      Term.new(value: "read", label: "read-only"),
      Term.new(value: "contribute", label: "read and contribute"),
    ].freeze

    # scope — what of a collection crosses.
    SCOPES = [
      Term.new(value: "all", label: "everything"),
      Term.new(value: "filtered", label: "a filtered set"),
      Term.new(value: "picked", label: "a hand-picked set"),
    ].freeze

    # provenance — how words carried across a peering are marked.
    PROVENANCE = [
      Term.new(value: "own", label: "written here"),
      Term.new(value: "firsthand", label: "firsthand"),
      Term.new(value: "withheld", label: "withheld"),
    ].freeze

    AXES = { "mode" => MODES, "scope" => SCOPES, "provenance" => PROVENANCE }.freeze

    module_function

    # The terms of one axis ("mode" | "scope" | "provenance"); raises on an
    # unknown axis, since that is a programming error, not bad input.
    def axis(name) = AXES.fetch(name.to_s)

    # The wire values of one axis, in canonical order.
    def values(name) = axis(name).map(&:value)

    # Is value a term of this axis?
    def valid?(name, value) = values(name).include?(value.to_s)

    # The term for a value, or nil.
    def term(name, value) = axis(name).find { |t| t.value == value.to_s }

    # The human label for a value, or the value itself if unknown.
    def label(name, value) = term(name, value)&.label || value.to_s
  end
end
