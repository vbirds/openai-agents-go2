package app

import "errors"

var errNotFound = errors.New("not found")

type User struct{ Name string }

func Greet(id string) string {
	user, _ := findUser(id)
	return "hello " + user.Name
}

func findUser(id string) (*User, error) {
	if id == "" {
		return nil, errNotFound
	}
	return &User{Name: id}, nil
}
