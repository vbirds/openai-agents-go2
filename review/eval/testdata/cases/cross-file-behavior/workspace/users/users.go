package users

type User struct {
	ID     int
	Name   string
	Active bool
}

type Service struct {
	users []User
}

// ListUsers returns all users in the system.
func (s *Service) ListUsers() []User {
	var out []User
	for _, u := range s.users {
		if !u.Active {
			continue
		}
		out = append(out, u)
	}
	return out
}
