-- people.directory (docs/signin-contract.md): the household's people,
-- as a peer may see them, become a capability of their own rather than
-- riding on chat. Every peer that could already see them through chat keeps
-- seeing exactly whom it saw: chat's use-side choice of people moves over.
INSERT INTO fed_capabilities (peer_id, capability, direction, config)
SELECT g.peer_id, 'people.directory', 'grant',
       COALESCE((SELECT u.config FROM fed_capabilities u
                 WHERE u.peer_id = g.peer_id AND u.capability = 'chat' AND u.direction = 'use'
                   AND json_extract(u.config, '$.people') IS NOT NULL), '{"people":"members"}')
FROM fed_capabilities g
WHERE g.capability = 'chat' AND g.direction = 'grant'
ON CONFLICT(peer_id, capability, direction) DO NOTHING;
