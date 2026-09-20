-- name: CreateScreening :one
-- price_cents is stated explicitly on every insert (migration 00008
-- dropped the column DEFAULT on purpose: no silent implicit prices).
INSERT INTO screenings (movie_id, starts_at, rows, seats_per_row, price_cents)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetScreening :one
SELECT * FROM screenings WHERE id = $1;

-- name: ListScreeningsByMovie :many
SELECT * FROM screenings
WHERE movie_id = $1
ORDER BY starts_at;
