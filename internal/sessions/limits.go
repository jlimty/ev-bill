package sessions

// ListLimit is how many sessions the list endpoints return: the API and status
// page show the newest 50 sessions.
const ListLimit = 50

// MaxReadings is the most readings one session may hold. It bounds the work of
// each page view and each row-locked write; 10,000 is about one reading every
// 17 s for 48 h.
const MaxReadings = 10_000
