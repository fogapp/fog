-- Lookups by person that walked whole tables: a person's rooms (the room
-- list, the directory, notification fan-out) filter chat_participants by
-- address and whether they left, and signing a person out everywhere or
-- listing their sessions filters people_sessions by person.
CREATE INDEX IF NOT EXISTS idx_chat_participants_person ON chat_participants(person_addr, left_seq);
CREATE INDEX IF NOT EXISTS idx_people_sessions_person ON people_sessions(person_id);
