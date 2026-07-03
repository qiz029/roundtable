package roundtable

import "database/sql"

type currentUser struct {
	ID              string
	Email           string
	DisplayName     string
	EmailVerifiedAt sql.NullString
	Status          string
}

type currentAgent struct {
	ID          string
	OwnerID     string
	Name        string
	Description string
}
