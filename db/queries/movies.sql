-- name: CreateMovie :one
INSERT INTO movies (title, synopsis, duration_minutes)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetMovie :one
SELECT * FROM movies WHERE id = $1;

-- name: ListMovies :many
SELECT * FROM movies ORDER BY title;
