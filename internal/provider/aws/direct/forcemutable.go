package direct

// ForceMutableForTest marks typeName's compiled reader Mutable, so a test
// of a caller in another package can reach the direct path of a type that
// lacks lifecycle evidence; restore undoes it.
func ForceMutableForTest(typeName string) (restore func()) {
	r := readers[typeName]
	forced := r
	forced.Mutable = true
	readers[typeName] = forced
	return func() { readers[typeName] = r }
}
