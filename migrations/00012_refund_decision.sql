-- +goose Up
-- One full-refund authorization per capture. payment_id also provides a
-- stable identity for the future provider request.
CREATE TABLE refund_requests (
    payment_id uuid PRIMARY KEY REFERENCES payments(id),
    amount_cents integer NOT NULL CHECK (amount_cents >= 0),
    currency text NOT NULL,
    requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    requested_by text NOT NULL DEFAULT current_user
);

-- Enforce the shared lock even when an older API performs the insert.
-- +goose StatementBegin
CREATE FUNCTION require_captured_booking_payment() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM payments
        WHERE booking_session_id = NEW.session_id AND status = 'captured'
        FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'booking requires captured payment' USING ERRCODE = 'BC001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER check_booking_payment BEFORE INSERT ON confirmed_bookings
FOR EACH ROW EXECUTE FUNCTION require_captured_booking_payment();

-- +goose StatementBegin
CREATE FUNCTION request_sandbox_refund(reviewed_payment_id uuid) RETURNS text
LANGUAGE plpgsql VOLATILE AS $$
DECLARE
    p payments%ROWTYPE;
BEGIN
    -- The post-lock booking lookup must see a confirmation that committed
    -- while this transaction waited for its payment lock.
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'refund review requires READ COMMITTED isolation';
    END IF;
    SELECT * INTO p FROM payments WHERE id = reviewed_payment_id FOR UPDATE;
    IF NOT FOUND THEN RETURN 'payment_not_found'; END IF;
    IF p.gateway <> 'fake' THEN RETURN 'unsupported_gateway'; END IF;
    IF EXISTS (SELECT 1 FROM refund_requests WHERE payment_id = p.id) THEN
        RETURN 'already_requested';
    END IF;
    IF EXISTS (SELECT 1 FROM confirmed_bookings WHERE session_id = p.booking_session_id) THEN
        RETURN 'booking_exists';
    END IF;
    IF p.status <> 'captured' THEN RETURN 'not_captured'; END IF;
    IF p.hold_expires_at IS NULL THEN RETURN 'unknown_deadline'; END IF;
    IF p.hold_expires_at > clock_timestamp() THEN RETURN 'before_deadline'; END IF;

    UPDATE payments SET status = 'refunding', updated_at = clock_timestamp() WHERE id = p.id;
    INSERT INTO refund_requests (payment_id, amount_cents, currency)
        VALUES (p.id, p.amount_cents, p.currency);
    RETURN 'requested';
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION request_sandbox_refund(uuid) FROM PUBLIC;

-- +goose Down
DROP FUNCTION request_sandbox_refund(uuid);
DROP TRIGGER check_booking_payment ON confirmed_bookings;
DROP FUNCTION require_captured_booking_payment();
DROP TABLE refund_requests;
