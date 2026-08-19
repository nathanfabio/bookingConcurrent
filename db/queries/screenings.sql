-- name: CreateScreening :one
INSERT INTO screenings (movie_id, starts_at, rows, seats_per_row)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetScreening :one
SELECT * FROM screenings WHERE id = $1;

-- name: ListScreeningsByMovie :many
SELECT * FROM screenings
WHERE movie_id = $1
ORDER BY starts_at;
