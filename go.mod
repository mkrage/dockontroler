module github.com/mkrage/dockontroler

// 1.22 is the minimum: the routing patterns used in internal/web/server.go
// ("POST /containers/{id}/start") were added to net/http in that release.
go 1.22
