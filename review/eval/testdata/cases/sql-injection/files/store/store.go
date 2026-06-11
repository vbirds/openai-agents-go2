package store

import "database/sql"

type Store struct{ db *sql.DB }

func (s *Store) UserByName(name string) (*User, error) {
	row := s.db.QueryRow("SELECT id, name FROM users WHERE name = '" + name + "'")
	u := &User{}
	if err := row.Scan(&u.ID, &u.Name); err != nil {
		return nil, err
	}
	return u, nil
}

type User struct {
	ID   int
	Name string
}
