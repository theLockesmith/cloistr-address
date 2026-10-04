-- Migration 011: ensure_user() — let services create a bare platform user row
--
-- Blossom refuses uploads from fresh keys: has_service_access() needs a
-- public.users row, and cloistr_blossom (correctly) has only SELECT on users.
-- ensure_user() is a narrow SECURITY DEFINER door: it inserts the pubkey and
-- nothing else, so enabled / is_platform_admin keep their column defaults and
-- an existing row (including a disabled one) is left untouched.
--
-- Callers: the service roles that hold EXECUTE on has_service_access() in
-- production (measured 2026-10-04): cloistr_blossom, coldforge_signer,
-- coldforge_relay. cloistr owns the function, so it needs no grant.
--
-- OWNERSHIP: runs as DB user `cloistr`, so the function is cloistr-owned, not
-- postgres-owned like has_service_access(). That is sufficient and narrower:
-- cloistr already holds INSERT on users, and SECURITY DEFINER runs with the
-- owner's rights. Idempotent, no transaction wrapper (005 lesson). Grants are
-- guarded so a self-hosted install without these roles still applies cleanly.

CREATE OR REPLACE FUNCTION public.ensure_user(p_pubkey text)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF p_pubkey IS NULL OR p_pubkey !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'ensure_user: pubkey must be 64 lowercase hex characters'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    INSERT INTO public.users (pubkey) VALUES (p_pubkey)
    ON CONFLICT (pubkey) DO NOTHING;
END;
$$;

REVOKE ALL ON FUNCTION public.ensure_user(text) FROM PUBLIC;

DO $$
DECLARE
    r text;
BEGIN
    FOREACH r IN ARRAY ARRAY['cloistr_blossom', 'coldforge_signer', 'coldforge_relay'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION public.ensure_user(text) TO %I', r);
        END IF;
    END LOOP;
END;
$$;
