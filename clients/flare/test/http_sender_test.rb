# frozen_string_literal: true

require_relative "test_helper"
require "socket"

# Wire-protocol coverage for HttpSender. Flare's whole value is Sentry
# wire-compatibility, and delivery failures are swallowed by design — a
# framing regression would otherwise ship silently. Real sockets: each test
# runs one request against a one-shot TCP server (fog-client's serve_once
# pattern) and asserts on the raw bytes the sender put on the wire.
class HttpSenderTest < Minitest::Test
  # A one-request HTTP server: canned response, records the request line,
  # headers, and body. Stdlib only. Returns [server, port, captured, thread];
  # the caller owns the server (close it in ensure) and must join the thread
  # before reading captured — the join is the barrier that makes the
  # cross-thread read safe.
  def serve_once(status: 200, headers: {})
    server = TCPServer.new("127.0.0.1", 0)
    port = server.addr[1]
    captured = +""
    thread = Thread.new do
      sock = server.accept
      content_length = 0
      while (line = sock.gets) && line != "\r\n"
        captured << line
        content_length = line.split(":", 2)[1].to_i if line.match?(/\AContent-Length:/i)
      end
      captured << "\r\n"
      captured << sock.read(content_length) if content_length.positive?
      extra = headers.map { |k, v| "#{k}: #{v}\r\n" }.join
      sock.write("HTTP/1.1 #{status} X\r\n#{extra}Content-Length: 0\r\nConnection: close\r\n\r\n")
      sock.close
    end
    [server, port, captured, thread]
  end

  # DSN with a path prefix on purpose: per parse_dsn, the last path segment
  # is the project id and everything before it is a prefix, so the derived
  # endpoint is /ingest/api/42/envelope/ — a test with a bare /42 DSN would
  # miss a prefix-handling regression.
  def config_for(port)
    c = Flare::Config.new
    c.dsn = "http://pubkey123@127.0.0.1:#{port}/ingest/42"
    c
  end

  # Sends one event straight through HttpSender (no Transport, no threads to
  # race) and returns [sender_result, request_head, envelope_body].
  def send_event(event, status: 200, headers: {})
    server, port, captured, thread = serve_once(status: status, headers: headers)
    result = Flare::HttpSender.new(config_for(port)).call(event)
    assert thread.join(5), "server thread did not finish"
    head, body = captured.split("\r\n\r\n", 2)
    [result, head, (body || +"").force_encoding(Encoding::UTF_8)]
  ensure
    server&.close
  end

  def test_envelope_is_three_newline_terminated_lines
    result, _head, body = send_event({ event_id: "abc123", message: "boom" })
    assert_nil result, "a 200 response must not ask the worker to pause"
    lines = body.split("\n", -1)
    assert_equal 4, lines.size, "envelope must be exactly three newline-terminated lines"
    assert_equal "", lines[3], "envelope must end with a trailing newline"

    header = JSON.parse(lines[0])
    assert_equal "abc123", header["event_id"]
    refute_nil Time.iso8601(header["sent_at"]), "sent_at must be ISO8601"

    item = JSON.parse(lines[1])
    assert_equal "event", item["type"]
    assert_equal lines[2].bytesize, item["length"]

    payload = JSON.parse(lines[2])
    assert_equal "boom", payload["message"]
    assert_equal "abc123", payload["event_id"]
  end

  def test_item_length_counts_bytes_not_characters
    event = { event_id: "ee", message: "schrödinger sagt — ünïcode ☃ 日本語" }
    _result, _head, body = send_event(event)
    lines = body.split("\n", -1)
    item = JSON.parse(lines[1])
    assert_equal lines[2].bytesize, item["length"]
    assert_operator item["length"], :>, lines[2].size,
      "with multibyte UTF-8 in the payload, length must be bytes (> characters)"
  end

  def test_auth_header_carries_dsn_public_key
    _result, head, _body = send_event({ event_id: "aa" })
    auth = head.lines.find { |l| l.match?(/\AX-Sentry-Auth:/i) }
    refute_nil auth, "request must carry X-Sentry-Auth"
    assert_includes auth, "sentry_key=pubkey123"
    assert_includes auth, "sentry_version=7"
  end

  def test_posts_envelope_to_project_ingest_path
    _result, head, _body = send_event({ event_id: "aa" })
    request_line = head.lines.first
    assert_equal "POST /ingest/api/42/envelope/ HTTP/1.1\r\n", request_line
    assert head.lines.any? { |l| l.match?(%r{\AContent-Type:\s*application/x-sentry-envelope}i) },
      "envelope must be posted as application/x-sentry-envelope"
  end

  def test_429_retry_after_is_parsed_and_clamped
    result, = send_event({ event_id: "aa" }, status: 429, headers: { "Retry-After" => "7" })
    assert_equal 7, result

    result, = send_event({ event_id: "aa" }, status: 429, headers: { "Retry-After" => "9999" })
    assert_equal 300, result, "Retry-After must clamp to the 300s ceiling"

    result, = send_event({ event_id: "aa" }, status: 429)
    assert_equal 60, result, "a 429 without Retry-After must fall back to 60s"

    result, = send_event({ event_id: "aa" }, status: 429, headers: { "Retry-After" => "junk" })
    assert_equal 1, result, "a junk Retry-After must still back off at least 1s"
  end

  def test_worker_pauses_before_next_delivery_after_429
    # The Transport half of the 429 contract: whatever seconds the sender
    # returns, the worker sleeps that long before popping the next event.
    # CollectingSender#call returns `pause` exactly like HttpSender returns a
    # parsed Retry-After. sleep guarantees at least the requested duration,
    # so the >= assertion is deterministic.
    sender = CollectingSender.new(pause: 0.3)
    t = Flare::Transport.new(Flare::Config.new, sender: sender)
    t.enqueue({ event_id: "a" })
    t.enqueue({ event_id: "b" })
    t.flush(timeout: 5)
    assert_equal %w[a b], sender.events.map { |e| e[:event_id] }
    assert_operator sender.times[1] - sender.times[0], :>=, 0.3,
      "worker must pause the full Retry-After before the next delivery"
  ensure
    t&.shutdown
  end
end
