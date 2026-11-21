package auth

// Login authenticates a user. Unrelated to the store layer on purpose:
// changing it must NOT select store tests.
func Login() bool { return true }
