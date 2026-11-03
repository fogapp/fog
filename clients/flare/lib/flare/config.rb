# frozen_string_literal: true

require "uri"

module Flare
  class Config
    # DSN shape (minted by Fog): http://<key>@host.docker.internal:8080/ingest/<app_id>
    # Sentry DSN semantics: the last path segment is the project id, anything
    # before it is a path prefix, and the derived ingest endpoint is
    # <scheme>://<host>/<prefix>/api/<project>/envelope/.
    ParsedDSN = Struct.new(:key, :endpoint)

    attr_accessor :dsn, :environment, :app_root, :logger,
                  :queue_size, :open_timeout, :read_timeout, :context_lines,
                  :rails_filters, :sender, :before_send
    attr_writer :release
    attr_reader :message_policy

    # Matched against param/context keys (case-insensitive) in addition to
    # whatever Rails' own filter_parameters contribute via the railtie.
    # "-" and "_" read alike, and may be left out: api_key, api-key,
    # X-Api-Key, apikey.
    SENSITIVE = "password|passwd|pwd|secret|token|api[-_]?key|access[-_]?key|private[-_]?key|auth|credential|cookie|session"
    FILTER_KEYS = /#{SENSITIVE}/io
    FILTERED = "[FILTERED]"

    def initialize
      @dsn = ENV["FLARE_DSN"]
      @environment = default_environment
      @app_root = default_root
      @queue_size = 100
      @open_timeout = 3
      @read_timeout = 5
      @context_lines = 3
      @rails_filters = []
      @release = nil
      @logger = nil
      @before_send = nil
      @message_policy = :full
    end

    # :full reports messages. :class_only reports none, unless the exception
    # answers true to flare_safe_message? (see ClassOnly). Only :full, or
    # nothing, turns messages on: a mistyped policy fails shut, because the
    # app that sets one has said its messages must not leave.
    def message_policy=(policy)
      @message_policy = [:full, "full", nil].include?(policy) ? :full : :class_only
    end

    def class_only?
      @message_policy == :class_only
    end

    def enabled?
      !parsed_dsn.nil?
    end

    # Memoized per raw DSN string: assigning a new dsn (e.g. from a Rails
    # initializer, after the railtie already asked enabled?) re-parses
    # instead of pinning the first answer forever.
    def parsed_dsn
      raw = dsn
      return @parsed_dsn if defined?(@parsed_dsn) && @parsed_dsn_raw == raw
      @parsed_dsn_raw = raw
      @parsed_dsn = parse_dsn(raw)
    end

    # Release identity, most reliable source first: the env Fog's builds
    # inject, then the REVISION file baked into the image. Computed once —
    # the answer can't change within a process, and events are built per
    # error, so per-event ENV reads and file stats would be pure overhead.
    # An explicit `config.release = ...` (attr_writer) still wins; setting
    # it back to nil re-derives.
    def release
      @release ||= compute_release
    end

    # Credentials, wherever they sit in a string (redact), whatever key
    # the string is under. Each pattern has a time limit: they run on the
    # thread that hit the error, over text anyone may have put there.
    #
    # A URL's userinfo, up to the last "@" before the next space, quote or
    # scheme, so a password holding "@" or "/" goes whole
    # (redis://:p@ss@host, postgres://u:p/ss@db/x) and a DSN's key with it.
    CREDENTIAL_URL = Regexp.new(%r{\b([a-z][a-z0-9+.\-]*://)(?:(?!://)[^\s"'<>])*@}i, timeout: 0.1)
    # An HTTP credential: "Bearer <token>", "Basic <base64>".
    CREDENTIAL_SCHEME = Regexp.new(%r{\b(Bearer|Basic|Token|Digest)(\s+)[A-Za-z0-9._~+/=\-]+}i, timeout: 0.1)
    # A sensitive key (SENSITIVE) given a value, in any of the shapes text
    # carries one: password=x (a query, a keyword DSN), "password":"x"
    # (JSON), X-Api-Key: x (a header, YAML), and Ruby's own inspect,
    # {"password" => "x"}, {password: "x"}, {:password=>"x"},
    # @password=#<Secret …>. The value goes whole: a quoted one keeps its
    # quotes; a balanced {…}, […] or #<…> goes with everything inside it
    # (auth: {user: …, pass: …}); one left unclosed goes to the end of its
    # line.
    QUOTED = %q{"(?:[^"\\\\]|\\\\.)*"|'(?:[^'\\\\]|\\\\.)*'}
    CREDENTIAL_PAIR = Regexp.new(
      "(?<![\\w.\\-\\[\\]:])(?<colon>:?)(?<q>[\"']?)(?<key>[\\w.\\-\\[\\]]*(?:#{SENSITIVE})[\\w.\\-\\[\\]]*)\\k<q>" \
      "(?<sep>\\s*(?:=>|[:=])\\s*)(?:(?:Bearer|Basic|Token|Digest)\\s+)?" \
      "(?<val>#{QUOTED}" \
      "|(?<cb>\\{(?:[^{}\"']|#{QUOTED}|\\g<cb>)*\\})" \
      "|(?<sb>\\[(?:[^\\[\\]\"']|#{QUOTED}|\\g<sb>)*\\])" \
      "|(?<ob>\\#<(?:[^<>\"']|#{QUOTED}|\\g<ob>)*>)" \
      "|(?:\\{|\\[|\\#<)[^\\n]*" \
      "|[^\\s;&,\"'}\\]]+)",
      Regexp::IGNORECASE, timeout: 0.1
    )
    # A cookie header's whole value, every cookie in it, to the line's end.
    CREDENTIAL_COOKIE = Regexp.new(%q{\b(?<name>(?:Set-)?Cookie)(?<sep>\s*:\s*)[^\r\n]*}, Regexp::IGNORECASE, timeout: 0.1)

    # Deep-scrub a params/context hash: a key matching the built-in patterns
    # or Rails' filter_parameters, by Rails' own rules (filtered_key?), has
    # its value replaced; every string left has its credentials redacted.
    def scrub(obj, parents = [])
      case obj
      when Hash
        obj.to_h { |k, v| [k, filtered_key?(k, parents) ? FILTERED : scrub(v, parents + [k.to_s])] }
      when Array
        obj.map { |v| scrub(v, parents) }
      when String
        redact(obj)
      else
        obj
      end
    end

    # A string with its credentials (CREDENTIAL_URL, CREDENTIAL_COOKIE,
    # CREDENTIAL_PAIR, CREDENTIAL_SCHEME) replaced by FILTERED. A string the patterns can't
    # read (invalid bytes) or can't finish in time goes whole.
    def redact(str)
      str.to_s
         .gsub(CREDENTIAL_URL) { "#{Regexp.last_match(1)}#{FILTERED}@" }
         .gsub(CREDENTIAL_COOKIE) { "#{Regexp.last_match(:name)}#{Regexp.last_match(:sep)}#{FILTERED}" }
         .gsub(CREDENTIAL_PAIR) do
           m = Regexp.last_match
           v = m[:val]
           q = v[0] if v.start_with?('"', "'")
           "#{m[:colon]}#{m[:q]}#{m[:key]}#{m[:q]}#{m[:sep]}#{q}#{FILTERED}#{q}"
         end
         .gsub(CREDENTIAL_SCHEME) { "#{Regexp.last_match(1)}#{Regexp.last_match(2)}#{FILTERED}" }
    rescue ArgumentError, Encoding::CompatibilityError, Regexp::TimeoutError
      FILTERED
    end

    # Whether key, under the keys parents (outermost first), is filtered:
    # by the built-in patterns, or by Rails' filter_parameters as
    # ActiveSupport::ParameterFilter reads them. A String or Symbol matches
    # anywhere in the key, case-insensitively; a Regexp matches the key; and
    # either, when it contains a dot ("credit_card.number"), matches the
    # dotted path to the key instead. Proc filters rewrite values in Rails
    # and are left to Rails' own filtered_parameters/filtered_path.
    def filtered_key?(key, parents = [])
      k = key.to_s
      return true if FILTER_KEYS.match?(k)
      flat, deep = compiled_filters
      return true if flat.any? { |r| r.match?(k) }
      return false if deep.empty?
      full = (parents.map(&:to_s) + [k]).join(".")
      deep.any? { |r| r.match?(full) }
    end

    private

    # [flat, deep] regexps, compiled as ParameterFilter#compile_filters does,
    # recompiled when rails_filters changes.
    def compiled_filters
      return @compiled if @compiled_for == rails_filters
      strings, regexps, deep_strings, deep_regexps = [], [], [], []
      Array(rails_filters).each do |f|
        case f
        when Proc then next
        when Regexp then (f.to_s.include?("\\.") ? deep_regexps : regexps) << f
        else
          s = Regexp.escape(f.to_s)
          (s.include?("\\.") ? deep_strings : strings) << s
        end
      end
      regexps << Regexp.new(strings.join("|"), true) unless strings.empty?
      deep_regexps << Regexp.new(deep_strings.join("|"), true) unless deep_strings.empty?
      @compiled_for = Array(rails_filters).dup
      @compiled = [regexps, deep_regexps]
    end

    def compute_release
      return ENV["GIT_SHA"] if ENV["GIT_SHA"].to_s != ""
      return ENV["GIT_REV"] if ENV["GIT_REV"].to_s != ""
      rev_file = File.join(app_root, "REVISION")
      return File.read(rev_file).strip if File.exist?(rev_file)
      ""
    rescue StandardError
      ""
    end

    def parse_dsn(raw)
      return nil if raw.nil? || raw.empty?
      u = URI.parse(raw)
      key = u.user
      project = File.basename(u.path.to_s)
      return nil unless key && !key.empty? && project.match?(/\A\d+\z/)
      prefix = File.dirname(u.path)
      prefix = "" if prefix == "/" || prefix == "."
      endpoint = "#{u.scheme}://#{u.host}:#{u.port}#{prefix}/api/#{project}/envelope/"
      ParsedDSN.new(key, endpoint)
    rescue URI::Error
      nil
    end

    def default_environment
      if defined?(Rails) && Rails.respond_to?(:env)
        Rails.env.to_s
      else
        ENV["RAILS_ENV"] || ENV["RACK_ENV"] || "production"
      end
    end

    def default_root
      if defined?(Rails) && Rails.respond_to?(:root) && Rails.root
        Rails.root.to_s
      else
        Dir.pwd
      end
    end
  end
end
