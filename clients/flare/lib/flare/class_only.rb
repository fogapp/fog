# frozen_string_literal: true

module Flare
  # What an event may hold under `config.message_policy = :class_only`, for an
  # app whose reports must carry no content. It keeps a list of what may leave
  # rather than a list of what may not: an exception's message can be copied
  # into anything an event carries (context, a breadcrumb, a tag, a request
  # parameter), and no search for the text finds a truncated or rephrased copy
  # of it.
  #
  # A report keeps where and what kind of failure it was: the exception's
  # class, its stack frames without their source lines, the action or job, the
  # HTTP method, and when and where it ran. An exception keeps its message only
  # if it answers true to flare_safe_message?.
  #
  # Every value that leaves is one this library derived or checked, never one
  # a before_send hook wrote. The built event passes through restrict once,
  # which checks the shape of what the builder took from the runtime (a frame's
  # function must read as a method name, a mechanism as a mechanism). A hook
  # then gets that event, and its answer passes through restrict again,
  # against a frozen copy of what was built: a value survives only where it
  # equals one the library built for that field, and what goes out is the
  # library's copy. A hook can drop an event, drop a field, drop or reorder
  # exceptions and frames, and set the level to one of LEVELS; whatever else
  # it changes is left out.
  module ClassOnly
    TOP_LEVEL = %i[event_id timestamp platform level server_name environment release transaction exception request extra].freeze
    FRAME = %i[filename abs_path function lineno in_app].freeze
    JOB = %i[class queue job_id].freeze
    # The top-level values a hook may keep but not change.
    BUILT = %i[event_id timestamp platform server_name environment release transaction].freeze
    LEVELS = %w[fatal error warning info debug].freeze
    # Sentry's own mechanism names look like this. Anything else a caller
    # passed as source: could be a sentence.
    MECHANISM = /\A[\w.:-]{1,64}\z/
    # A Ruby frame label: a method, perhaps with its owner (Foo::Bar#baz,
    # Foo.baz), perhaps inside a block, rescue or ensure, or one of the
    # interpreter's own (<main>, <top (required)>, <class:Foo>). A label that
    # doesn't read like one of these is dropped from its frame.
    FUNCTION = %r{\A
      (?:(?:block(?:\ \(\d{1,4}\ levels\))?|rescue|ensure)\ in\ )*
      (?:
        <(?:main|top\ \(required\)|(?:class|module):[A-Z]\w*|internal:\w+)>
      | singleton\ class
      | (?:[A-Z]\w*(?:::[A-Z]\w*)*[\#.])?
        (?:[A-Za-z_]\w*[?!=]?|\[\]=?|<=>|===?|=~|![=~]?|[-+]@|\*\*|<<|>>|<=|>=|[-+*/%<>&|^~`])
      )\z}x
    MAX_FUNCTION = 256
    HTTP_METHOD = /\A[A-Z]{1,16}\z/
    # A constant (a job's class) or a controller action, as the builder
    # writes a transaction: "PostsController#show", "Billing::SyncJob".
    CONSTANT = /\A[A-Z]\w*(?:::[A-Z]\w*)*\z/
    TRANSACTION = /\A[A-Z]\w*(?:::[A-Z]\w*)*(?:#[A-Za-z_]\w*[?!]?)?\z/
    # A queue's name, a job's id: one plain identifier.
    IDENTIFIER = /\A[A-Za-z0-9_][\w.-]{0,63}\z/
    # A source path below one of its roots: segments of word characters,
    # dots, dashes, @ and +, no "..", no spaces, nothing else.
    PATH = %r{\A(?:[\w@+-][\w.@+-]*|\.[\w@+-][\w.@+-]*)(?:/(?:[\w@+-][\w.@+-]*|\.[\w@+-][\w.@+-]*))*\z}
    INTERNAL = /\A<internal:[\w\/]+>\z/
    MAX_PATH = 1024
    # What a frame's file reads when its path isn't a source file's:
    # eval(code, binding, "Dear Bob, …") names its frames whatever it likes.
    EVAL = "(eval)"
    # Whether a path is a file on disk, remembered (FIFO) so a hot error
    # doesn't stat every frame every time.
    EXISTS = {}
    EXISTS_MAX = 2000
    EXISTS_MUTEX = Mutex.new
    MISSING = Object.new.freeze
    private_constant :MISSING

    # The message an exception may report, or "".
    # The caller cuts it to length, after redacting it.
    def self.message_for(exception, limit = nil)
      return "" unless exception.respond_to?(:flare_safe_message?) && exception.flare_safe_message? == true
      limit ? exception.message.to_s[0, limit] : exception.message.to_s
    rescue StandardError
      ""
    end

    # What a built event said, frozen apart from it (a hook is handed the
    # event itself, and can write on it), with a table per field of the
    # values it may carry.
    Allowed = Struct.new(:event, :messages, :types, :mechanisms, :frames)

    def self.allowed(event)
      built = frozen_copy(event)
      values = entries(built)
      frames = FRAME.to_h { |k| [k, {}] }
      values.each do |v|
        Array(hash(v[:stacktrace])[:frames]).each do |f|
          FRAME.each { |k| frames[k][f[k]] = f[k] if f.key?(k) }
        end
      end
      Allowed.new(
        built,
        table(values.map { |v| v[:value] }.reject { |m| m.to_s.empty? }),
        table(values.map { |v| v[:type] }.compact),
        table(values.filter_map { |v| hash(v[:mechanism])[:type] }),
        frames,
      )
    end

    # The part of event allowed out. Without allowed:, event is one the
    # builder just made; with it, event is a hook's answer, held to what was
    # built.
    # roots are the directories a source path may sit under (path_roots).
    def self.restrict(event, allowed: nil, roots: path_roots(nil))
      allowed ? recheck(event, allowed) : first_pass(event, roots)
    end

    # The app's root, then where gems and Ruby's own library live.
    def self.path_roots(app_root)
      roots = [app_root]
      roots.concat(Gem.path.map { |d| File.join(d, "gems") }) if defined?(Gem)
      roots << RbConfig::CONFIG["rubylibdir"] << RbConfig::CONFIG["rubyarchdir"] if defined?(RbConfig)
      roots.compact.map { |r| r.to_s.chomp("/") }.reject(&:empty?).uniq
    end

    def self.first_pass(event, roots)
      out = event.slice(*TOP_LEVEL)
      out.delete(:transaction) unless out[:transaction].is_a?(String) && TRANSACTION.match?(out[:transaction])
      out[:exception] = { values: entries(event).map { |v| checked_value(v, roots) } }
      method = hash(out.delete(:request))[:method]
      out[:request] = { method: method } if method.is_a?(String) && HTTP_METHOD.match?(method)
      job = hash(hash(out.delete(:extra))[:job]).slice(*JOB)
      job.delete(:class) unless job[:class].is_a?(String) && CONSTANT.match?(job[:class])
      job.delete(:queue) unless job[:queue].is_a?(String) && IDENTIFIER.match?(job[:queue])
      job.delete(:job_id) unless job[:job_id].is_a?(String) && IDENTIFIER.match?(job[:job_id])
      out[:extra] = { job: job } unless job.empty?
      ordered(out)
    end
    private_class_method :first_pass

    # A frame's path, if it is a source file's: Ruby's own <internal:…>, or
    # an existing file under one of roots whose path below the root is
    # path-shaped. Anything else is EVAL.
    def self.source_path(path, roots)
      return path if path.is_a?(String) && INTERNAL.match?(path)
      return EVAL unless path.is_a?(String) && path.length <= MAX_PATH

      root = roots.find { |r| path.start_with?("#{r}/") }
      return EVAL unless root && PATH.match?(path.delete_prefix("#{root}/"))

      file?(path) ? path : EVAL
    end

    def self.file?(path)
      EXISTS_MUTEX.synchronize do
        return EXISTS[path] if EXISTS.key?(path)

        EXISTS.shift if EXISTS.size >= EXISTS_MAX
        EXISTS[path] = File.file?(path)
      end
    rescue StandardError
      false
    end
    private_class_method :file?

    def self.checked_value(v, roots)
      value = v.slice(:type, :value)
      if (m = hash(v[:mechanism])[:type])
        value[:mechanism] = hash(v[:mechanism]).slice(:handled).merge(type: MECHANISM.match?(m.to_s) ? m : "generic")
      end
      if (frames = hash(v[:stacktrace])[:frames])
        value[:stacktrace] = {
          frames: Array(frames).map do |f|
            frame = hash(f).slice(*FRAME)
            frame.delete(:function) unless function?(frame[:function])
            abs = source_path(frame[:abs_path], roots)
            frame[:abs_path] = abs if frame.key?(:abs_path)
            if frame.key?(:filename)
              # The builder's filename is abs_path, or its tail below the
              # app root; it stands only where abs_path did.
              name = frame[:filename]
              frame[:filename] = abs != EVAL && name.is_a?(String) && abs.end_with?(name) &&
                                 (name == abs || (PATH.match?(name) && abs.end_with?("/#{name}"))) ? name : EVAL
            end
            frame
          end,
        }
      end
      value
    end
    private_class_method :checked_value

    def self.function?(label)
      label.is_a?(String) && label.length <= MAX_FUNCTION && FUNCTION.match?(label)
    end

    def self.recheck(answer, allowed)
      built = allowed.event
      out = {}
      BUILT.each do |k|
        kept = lookup(table([built[k]].compact), answer[k]) if built.key?(k)
        out[k] = kept unless kept.nil? || kept.equal?(MISSING)
      end
      level = LEVELS.find { |l| l.eql?(answer[:level]) || l.to_sym.equal?(answer[:level]) }
      out[:level] = level if level
      out[:exception] = { values: entries(answer).map { |v| recheck_value(v, allowed) } }
      method = hash(built[:request])[:method]
      out[:request] = { method: method } if method && method.eql?(hash(answer[:request])[:method])
      built_job = hash(hash(built[:extra])[:job])
      answer_job = hash(hash(answer[:extra])[:job])
      job = JOB.each_with_object({}) do |k, h|
        h[k] = built_job[k] if built_job.key?(k) && built_job[k].eql?(answer_job[k])
      end
      out[:extra] = { job: job } unless job.empty?
      ordered(out)
    end
    private_class_method :recheck

    def self.recheck_value(v, allowed)
      value = {}
      type = lookup(allowed.types, v[:type])
      value[:type] = type unless type.equal?(MISSING)
      message = lookup(allowed.messages, v[:value])
      value[:value] = message.equal?(MISSING) ? "" : message
      if v.key?(:mechanism)
        m = hash(v[:mechanism])
        mtype = lookup(allowed.mechanisms, m[:type])
        mechanism = { type: mtype.equal?(MISSING) ? "generic" : mtype }
        mechanism[:handled] = m[:handled] if m[:handled] == true || m[:handled] == false
        value[:mechanism] = mechanism
      end
      if (frames = hash(v[:stacktrace])[:frames])
        kept = Array(frames).filter_map do |f|
          f = hash(f)
          frame = {}
          FRAME.each do |k|
            next unless f.key?(k)
            x = lookup(allowed.frames[k], f[k])
            frame[k] = x unless x.equal?(MISSING)
          end
          frame unless frame.empty?
        end
        value[:stacktrace] = { frames: kept }
      end
      value
    end
    private_class_method :recheck_value

    # The library's own copy of value from table, or MISSING. Only exact
    # matches count: same class, same contents (so 1.0 is not 1, and a
    # String subclass is not a String).
    def self.lookup(table, value)
      return MISSING unless [String, Integer, TrueClass, FalseClass].any? { |c| value.instance_of?(c) }
      table.key?(value) ? table[value] : MISSING
    end
    private_class_method :lookup

    def self.table(values)
      values.to_h { |v| [v, v] }
    end
    private_class_method :table

    def self.ordered(out)
      TOP_LEVEL.each_with_object({}) { |k, h| h[k] = out[k] if out.key?(k) }
    end
    private_class_method :ordered

    # A deep, frozen copy of a built event: plain hashes, arrays and scalars.
    def self.frozen_copy(obj)
      case obj
      when Hash then obj.to_h { |k, v| [k, frozen_copy(v)] }.freeze
      when Array then obj.map { |v| frozen_copy(v) }.freeze
      when String then obj.dup.freeze
      else obj
      end
    end
    private_class_method :frozen_copy

    def self.entries(event)
      Array(hash(event[:exception])[:values]).select { |v| v.is_a?(Hash) }
    end
    private_class_method :entries

    def self.hash(obj)
      obj.is_a?(Hash) ? obj : {}
    end
    private_class_method :hash
  end
end
