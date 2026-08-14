package repository

import "net/url"

func Display(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.User == nil {
		return value
	}
	username := u.User.Username()
	_, password := u.User.Password()
	if u.Scheme == "http" || u.Scheme == "https" {
		u.User = nil
	} else if password {
		u.User = url.User(username)
	}
	return u.String()
}
