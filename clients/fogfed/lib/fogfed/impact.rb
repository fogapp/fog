# frozen_string_literal: true

module Fogfed
  # The consent grammar's one visible surface: the impact sentence. A share is
  # a lens — who sees what, in how much detail, and what never crosses — and
  # the sentence is that lens read back in plain English. fog and every app
  # that shares assemble it the same way so a person reads the same shape of
  # promise everywhere; the conformance vectors prove Go and Ruby agree.
  #
  #   <subject> sees <scope> — <sees>.
  #   [ They do not see <hidden>.]
  #   [ Never shared: <floor>.]
  #   [ <addendum>.]
  #
  # scope, floor, addendum and subject are phrases the caller composes for its
  # own content; sees and hidden are lists this joins.
  module Impact
    module_function

    # Join a list the way the sentence reads: "" / "a" / "a and b" /
    # "a, b, and c" (Oxford comma) — matching Rails' Array#to_sentence.
    def to_sentence(items)
      items = Array(items).map(&:to_s).reject(&:empty?)
      case items.length
      when 0 then ""
      when 1 then items[0]
      when 2 then "#{items[0]} and #{items[1]}"
      else "#{items[0..-2].join(", ")}, and #{items[-1]}"
      end
    end

    def sentence(subject:, scope:, sees:, hidden: [], floor: nil, addendum: nil)
      out = +"#{subject} sees #{scope} — #{to_sentence(sees)}."
      shown_hidden = to_sentence(hidden)
      out << " They do not see #{shown_hidden}." unless shown_hidden.empty?
      out << " Never shared: #{floor}." if floor && !floor.to_s.empty?
      out << " #{addendum}." if addendum && !addendum.to_s.empty?
      out
    end
  end
end
