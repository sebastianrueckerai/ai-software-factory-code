package factoryd

// The export surface of the freeze card's pg move: freezecard_pg_test.go
// moved into internal/factoryd/pg with the act family's split. Its one
// unexported reach, PGStore's db handle, already ships as DB() in
// pgexport.go, so this file adds no name of its own.
