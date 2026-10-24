-- Migration 014: seed system-reserved usernames.
--
-- ME-7: GET /api/v1/addresses/check/admin (and root, support, ...) answered
-- "Available!" in production. is_username_available() and AtomicRegisterAddress
-- both already refuse any username present in reserved_usernames with
-- reserved_for_pubkey IS NULL -- that part of the code was correct -- but
-- nothing anywhere in this codebase ever inserted the common system words
-- into that table. reserved_usernames only ever gained rows through the
-- admin "reserve for a pubkey" API, so the generic reserved list was simply
-- empty. This seeds it.
--
-- Idempotent (ON CONFLICT DO NOTHING on the primary key): the init container
-- reapplies every *_up.sql on each pod start, and an operator may have since
-- freed one of these names via the admin API, which this must not re-reserve.
INSERT INTO reserved_usernames (username, reserved_for_pubkey, reason) VALUES
    ('admin', NULL, 'system reserved'),
    ('administrator', NULL, 'system reserved'),
    ('root', NULL, 'system reserved'),
    ('support', NULL, 'system reserved'),
    ('help', NULL, 'system reserved'),
    ('info', NULL, 'system reserved'),
    ('contact', NULL, 'system reserved'),
    ('abuse', NULL, 'system reserved'),
    ('security', NULL, 'system reserved'),
    ('postmaster', NULL, 'system reserved'),
    ('webmaster', NULL, 'system reserved'),
    ('hostmaster', NULL, 'system reserved'),
    ('noreply', NULL, 'system reserved'),
    ('no-reply', NULL, 'system reserved'),
    ('moderator', NULL, 'system reserved'),
    ('mod', NULL, 'system reserved'),
    ('staff', NULL, 'system reserved'),
    ('system', NULL, 'system reserved'),
    ('www', NULL, 'system reserved'),
    ('mail', NULL, 'system reserved'),
    ('api', NULL, 'system reserved'),
    ('billing', NULL, 'system reserved'),
    ('cloistr', NULL, 'system reserved'),
    ('coldforge', NULL, 'system reserved')
ON CONFLICT (username) DO NOTHING;
