package billing

import "example.com/app/users"

// GenerateInvoices bills every user in the system. Suspended (inactive)
// users must still be invoiced for outstanding balances; deactivation does
// not waive debt.
func GenerateInvoices(svc *users.Service) []Invoice {
	var invoices []Invoice
	for _, u := range svc.ListUsers() {
		invoices = append(invoices, Invoice{UserID: u.ID})
	}
	return invoices
}

type Invoice struct{ UserID int }
