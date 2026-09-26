package direct

// readers is every compiled reader, by type: each generated
// readers_<service>.go registers its service's.
var readers = map[string]Reader{}

// register adds a generated file's readers.
func register(rs map[string]Reader) {
	for typeName, r := range rs {
		readers[typeName] = r
	}
}
