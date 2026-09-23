-- name: GetRecord :one
SELECT record FROM refs WHERE name = ?;

-- name: PutRecord :exec
INSERT INTO refs (name, record) VALUES (?, ?)
ON CONFLICT (name) DO UPDATE SET record = excluded.record;

-- name: CreateRecord :execrows
INSERT INTO refs (name, record) VALUES (?, ?)
ON CONFLICT (name) DO NOTHING;

-- name: ReplaceRecord :execrows
UPDATE refs SET record = sqlc.arg(record)
WHERE name = sqlc.arg(name) AND record = sqlc.arg(old_record);

-- name: DeleteRecord :execrows
DELETE FROM refs WHERE name = ?;

-- name: DeleteRecordIf :execrows
DELETE FROM refs WHERE name = sqlc.arg(name) AND record = sqlc.arg(old_record);

-- name: ListRecords :many
SELECT name, record FROM refs ORDER BY name;

-- name: DeleteAllRecords :exec
DELETE FROM refs;
