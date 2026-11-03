# frozen_string_literal: true

require "minitest/autorun"
require "socket"
require_relative "../lib/fog_client"

class FogClientTest < Minitest::Test
  def with_env(url, token)
    old = [ENV["FOG_API_URL"], ENV["FOG_API_TOKEN"]]
    ENV["FOG_API_URL"] = url
    ENV["FOG_API_TOKEN"] = token
    yield
  ensure
    ENV["FOG_API_URL"], ENV["FOG_API_TOKEN"] = old
  end

  # A one-request HTTP server: canned response, records the request line,
  # headers, and body. Stdlib only — webrick left the stdlib in ruby 3.0.
  # Returns [server, port, captured, thread]; the caller owns the server
  # (close it in ensure) and must join the thread before reading captured —
  # the join is the barrier that makes the cross-thread read safe.
  def serve_once(body_json, status: 200, raw_body: nil)
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
      captured << sock.read(content_length) if content_length.positive?
      payload = raw_body || JSON.generate(body_json)
      sock.write("HTTP/1.1 #{status} #{status == 200 ? 'OK' : 'NO'}\r\n" \
                 "Content-Type: application/json\r\n" \
                 "Content-Length: #{payload.bytesize}\r\nConnection: close\r\n\r\n#{payload}")
      sock.close
    end
    [server, port, captured, thread]
  end

  ONE_PEER = {
    "peers" => [{
      "name" => "dave", "instanceId" => "01X", "online" => true,
      "lastSeenAt" => "2026-08-13T00:00:00Z",
      "apps" => [{ "app" => "feed", "url" => "https://feed.dave.house", "contracts" => ["fog-connect/1"] }]
    }]
  }.freeze

  def test_inert_without_env
    with_env(nil, nil) do
      refute FogClient.available?
      assert_equal [], FogClient.connect.peers
    end
  end

  def test_inert_with_partial_env
    with_env("http://127.0.0.1:9", nil) do
      refute FogClient.available?
      assert_equal [], FogClient.connect.peers
    end
  end

  def test_peers_parse_and_bearer_auth
    server, port, captured, thread = serve_once(ONE_PEER)
    with_env("http://127.0.0.1:#{port}", "tok123") do
      assert FogClient.available?
      peers = FogClient.connect.peers
      assert thread.join(5), "server thread did not finish"
      assert_equal 1, peers.size
      p = peers.first
      assert_equal "dave", p.name
      # No label set on that peer: display_name falls back to the name, so a
      # UI can read display_name unconditionally.
      assert_equal "dave", p.display_name
      assert p.online?
      assert_equal "https://feed.dave.house", p.apps.first.url
      assert_equal ["fog-connect/1"], p.apps.first.contracts
      assert_includes captured, "GET /app/v1/connect/peers"
      assert_includes captured, "Authorization: Bearer tok123"
    end
  ensure
    server&.close
  end

  # The operator's label is what a person should see. name stays the
  # addressing key, so both travel and neither one stands in for the other.
  def test_label_becomes_display_name_without_moving_the_addressing_key
    server, port, _captured, thread = serve_once({
      "peers" => [{
        "name" => "dave", "displayName" => "Dave's house", "instanceId" => "01X",
        "online" => true, "apps" => []
      }]
    })
    with_env("http://127.0.0.1:#{port}", "tok") do
      p = FogClient.connect.peers.first
      assert thread.join(5), "server thread did not finish"
      assert_equal "Dave's house", p.display_name
      assert_equal "dave", p.name
    end
  ensure
    server&.close
  end

  # A fogd predating displayName answers with name alone; a UI reading
  # display_name must not go blank against one.
  def test_display_name_falls_back_on_an_older_fogd
    server, port, _captured, thread = serve_once({
      "peers" => [{ "name" => "dave", "instanceId" => "01X", "online" => false, "apps" => [] }]
    })
    with_env("http://127.0.0.1:#{port}", "tok") do
      p = FogClient.connect.peers.first
      assert thread.join(5), "server thread did not finish"
      assert_equal "dave", p.display_name
    end
  ensure
    server&.close
  end

  def test_unreachable_fog_is_an_empty_answer
    with_env("http://127.0.0.1:1", "tok") do
      assert_equal [], FogClient.connect.peers
    end
  end

  # FOG_API_URL may carry a path prefix (a fogd behind a reverse proxy).
  # URI.join would have discarded it — RFC 3986 resolution replaces the base
  # path with the absolute request path — turning every call into a 404 that
  # the nil-on-error rescue then disguises as "no peers", permanently.
  def test_url_path_prefix_is_preserved
    server, port, captured, thread = serve_once(ONE_PEER)
    with_env("http://127.0.0.1:#{port}/api/", "tok") do
      peers = FogClient.connect.peers
      assert thread.join(5), "server thread did not finish"
      assert_equal 1, peers.size
      assert_includes captured, "GET /api/app/v1/connect/peers"
    end
  ensure
    server&.close
  end

  def test_request_posts_json_and_parses_the_grant
    server, port, captured, thread = serve_once({
      "app" => "feed", "url" => "https://feed.dave.house",
      "credential" => { "token" => "minted" }, "payload" => { "hello" => "from-dave" },
      "peer" => { "instance" => "01X", "name" => "dave" }
    })
    with_env("http://127.0.0.1:#{port}", "tok") do
      grant = FogClient.connect.request(peer: "dave", app: "feed", public_key: "K", payload: { "hello" => "from-me" })
      assert thread.join(5), "server thread did not finish"
      assert_equal "feed", grant.app
      assert_equal "https://feed.dave.house", grant.url
      assert_equal({ "token" => "minted" }, grant.credential)
      assert_equal({ "hello" => "from-dave" }, grant.payload)
      assert_equal "01X", grant.peer_instance
      assert_equal "dave", grant.peer_name
      assert_includes captured, "POST /app/v1/connect/grants"
      assert_includes captured, "Content-Type: application/json"
      assert_includes captured, "Authorization: Bearer tok"
      assert_includes captured, JSON.generate(peer: "dave", app: "feed", publicKey: "K", payload: { "hello" => "from-me" })
    end
  ensure
    server&.close
  end

  # The target app defaults on the fog side, so a hook asking for its
  # sibling hook sends only the peer.
  def test_request_sends_only_what_was_given
    server, port, captured, thread = serve_once({ "app" => "hook", "credential" => { "token" => "t" } })
    with_env("http://127.0.0.1:#{port}", "tok") do
      grant = FogClient.connect.request(peer: "dave")
      assert thread.join(5), "server thread did not finish"
      assert_equal "hook", grant.app
      assert_nil grant.payload
      assert_includes captured, JSON.generate(peer: "dave")
    end
  ensure
    server&.close
  end

  def test_non_2xx_is_an_empty_answer
    server, port, _captured, thread = serve_once({ "error" => "boom" }, status: 500)
    with_env("http://127.0.0.1:#{port}", "tok") do
      assert_equal [], FogClient.connect.peers
      assert thread.join(5), "server thread did not finish"
    end
  ensure
    server&.close
  end

  def test_bad_json_is_an_empty_answer
    server, port, _captured, thread = serve_once(nil, raw_body: "<html>not json</html>")
    with_env("http://127.0.0.1:#{port}", "tok") do
      assert_equal [], FogClient.connect.peers
      assert thread.join(5), "server thread did not finish"
    end
  ensure
    server&.close
  end

  # Valid JSON of the wrong shape is a failure like any other: an empty
  # answer, never an exception in the app.
  def test_wrong_shaped_json_is_an_empty_answer
    [
      [[1, 2], ->(r) { r.nil? || r == [] }],
      [{ "peers" => ["dave", 7, nil, { "name" => "ok", "apps" => ["x", 1] }],
         "mounts" => ["m", 3], "tunnel" => "up" }, nil],
    ].each do |body, _|
      %i[peers mounts reachability].each do |call|
        server, port, _captured, thread = serve_once(body)
        with_env("http://127.0.0.1:#{port}", "tok") do
          got = case call
                when :peers then FogClient.connect.peers(fresh: true)
                when :mounts then FogClient.files.mounts
                when :reachability then FogClient.connect.reachability
                end
          case call
          when :peers
            assert_equal(body.is_a?(Hash) ? ["ok"] : [], got.map(&:name))
            assert_equal [], got.first.apps if body.is_a?(Hash)
          when :mounts then assert_equal [], got
          when :reachability
            body.is_a?(Hash) ? refute(got.tunnel_running) : assert_nil(got)
          end
          assert thread.join(5), "server thread did not finish"
        end
      ensure
        server&.close
      end
    end
  end

  def test_request_refusal_is_nil
    server, port, _captured, thread = serve_once({ "error" => "this peer may not connect to apps here" }, status: 502)
    with_env("http://127.0.0.1:#{port}", "tok") do
      assert_nil FogClient.connect.request(peer: "dave", app: "feed")
      assert thread.join(5), "server thread did not finish"
    end
  ensure
    server&.close
  end

  # An answer without a credential is not a grant, whatever else it says.
  def test_request_without_a_credential_is_nil
    server, port, _captured, thread = serve_once({ "app" => "feed", "payload" => { "only" => "handshake" } })
    with_env("http://127.0.0.1:#{port}", "tok") do
      assert_nil FogClient.connect.request(peer: "dave", app: "feed")
      assert thread.join(5), "server thread did not finish"
    end
  ensure
    server&.close
  end
  # The server answers exactly one request, so a second peers call within
  # the TTL proves the cache: it must not touch the network at all.
  def test_peers_are_cached_within_the_ttl
    server, port, _captured, thread = serve_once(ONE_PEER)
    with_env("http://127.0.0.1:#{port}", "tok") do
      first = FogClient.connect.peers
      assert thread.join(5), "server thread did not finish"
      server.close
      second = FogClient.connect.peers
      assert_equal 1, second.size
      assert_equal first.first.name, second.first.name
      # fresh: true bypasses the cache — and with the server gone, the
      # failure is an honest empty answer, never a stale cached one.
      assert_equal [], FogClient.connect.peers(fresh: true)
    end
  ensure
    server&.close
  end

  # A cached answer for one fog must not leak to another: the cache is
  # keyed by (url, token), so changing env is a miss, not a stale hit.
  def test_peers_cache_is_keyed_by_env
    server, port, _captured, thread = serve_once(ONE_PEER)
    with_env("http://127.0.0.1:#{port}", "tok") do
      assert_equal 1, FogClient.connect.peers.size
      assert thread.join(5), "server thread did not finish"
    end
    with_env("http://127.0.0.1:1", "other-tok") do
      assert_equal [], FogClient.connect.peers
    end
  ensure
    server&.close
  end

  def test_reachability_parses_the_verdict_and_auth
    server, port, captured, thread = serve_once({
      "reachable" => false, "via" => "none",
      "tunnel" => { "configured" => true, "running" => false },
      "advice" => "Start the tunnel."
    })
    with_env("http://127.0.0.1:#{port}", "tok123") do
      r = FogClient.connect.reachability
      assert thread.join(5), "server thread did not finish"
      refute r.reachable?
      assert_nil r.url
      assert_equal "none", r.via
      assert r.tunnel_configured
      refute r.tunnel_running
      assert_equal "Start the tunnel.", r.advice
      assert_includes captured, "GET /app/v1/connect/reachability"
      assert_includes captured, "Authorization: Bearer tok123"
    end
  ensure
    server&.close
  end

  def test_reachability_reachable_carries_the_url
    server, port, _captured, thread = serve_once({
      "reachable" => true, "url" => "https://fog.dave.house", "via" => "advertised",
      "tunnel" => { "configured" => false, "running" => false }
    })
    with_env("http://127.0.0.1:#{port}", "tok") do
      r = FogClient.connect.reachability
      assert thread.join(5), "server thread did not finish"
      assert r.reachable?
      assert_equal "https://fog.dave.house", r.url
      assert_equal "advertised", r.via
      assert_nil r.advice
    end
  ensure
    server&.close
  end

  # Outside fog there is no verdict to give: nil, never a guess, never a raise.
  def test_reachability_is_nil_without_fog
    with_env(nil, nil) do
      assert_nil FogClient.connect.reachability
    end
    with_env("http://127.0.0.1:1", "tok") do
      assert_nil FogClient.connect.reachability
    end
  end

  def test_files_mounts_parse_and_auth
    server, port, captured, thread = serve_once({
      "mounts" => [
        { "mount" => "Books", "hostPath" => "/data/books", "kind" => "peer",
          "peer" => "dave", "share" => "Library", "readOnly" => true,
          "hydrated" => false, "status" => "synced", "lastSyncedAt" => "2026-01-01T00:00:00Z" }
      ]
    })
    with_env("http://127.0.0.1:#{port}", "tok123") do
      mounts = FogClient.files.mounts
      assert thread.join(5), "server thread did not finish"
      assert_equal 1, mounts.size
      m = mounts.first
      assert_equal "Books", m.mount
      assert_equal "/data/books", m.host_path
      assert m.peer?
      assert_equal "dave", m.peer
      assert m.read_only
      assert_includes captured, "GET /app/v1/files/mounts"
      assert_includes captured, "Authorization: Bearer tok123"
    end
  ensure
    server&.close
  end

  def test_connections_parse_and_auth
    server, port, captured, thread = serve_once({
      "connections" => [
        { "app" => "sabnzbd", "url" => "http://sabnzbd:8080", "credential" => "k",
          "paths" => [{ "remote" => "/downloads", "local" => "/data/downloads" }], "unseen" => ["Private"] },
        { "app" => "stream", "problem" => "not installed" },
      ],
    })
    with_env("http://127.0.0.1:#{port}", "tok123") do
      conns = FogClient.connections
      assert thread.join(5), "server thread did not finish"
      sab, stream = conns
      assert sab.usable?
      assert_equal [{ "remote" => "/downloads", "local" => "/data/downloads" }], sab.paths
      assert_equal ["Private"], sab.unseen
      refute stream.installed?
      refute stream.usable?
      assert_includes captured, "GET /app/v1/connections"
      assert_includes captured, "Authorization: Bearer tok123"
    end
  ensure
    server&.close
  end

  def test_connections_nil_without_fog
    with_env(nil, nil) { assert_nil FogClient.connections }
  end

  def test_files_mounts_empty_without_fog
    with_env(nil, nil) do
      assert_equal [], FogClient.files.mounts
    end
  end

  # Records the bounds each Net::HTTP carries when it sends, while a test
  # has set FogClientTest.bounds to collect them.
  module RecordBounds
    def request(req, *rest, &blk)
      FogClientTest.bounds&.push([open_timeout, read_timeout, write_timeout])
      super
    end
  end
  Net::HTTP.prepend(RecordBounds)
  class << self
    attr_accessor :bounds
  end

  # Sign-in's discovery and token calls run on a web request like the rest
  # of the gem's, so they get the same bounds: its own Http used to leave
  # the send at Net::HTTP's default 60s.
  def test_signin_http_bounds_every_phase
    server, port, _captured, thread = serve_once({ "ok" => true })
    FogClientTest.bounds = []
    assert_equal({ "ok" => true }, FogClient::Signin::Http.new.get_json("http://127.0.0.1:#{port}/x"))
    assert thread.join(5), "server thread did not finish"
    refute_empty FogClientTest.bounds
    FogClientTest.bounds.each do |b|
      assert_equal [FogClient::OPEN_TIMEOUT, FogClient::READ_TIMEOUT, FogClient::WRITE_TIMEOUT], b
    end
  ensure
    FogClientTest.bounds = nil
    server&.close
  end

end
