package naming

// colors, adjectives, and animals are the JavaScript CLI's exact
// word lists, in the exact same order. GenerateEnvironmentName's output
// space must match 0.5.0 byte-for-byte: the index a word sits at has no
// meaning of its own, but keeping the lists identical means kraai never
// grows a name that 0.5.0 could never have produced.
var colors = []string{
	"blue", "red", "green", "amber", "violet", "coral", "teal", "slate",
	"crimson", "azure", "olive", "copper", "indigo", "scarlet", "jade",
}

var adjectives = []string{
	"honey", "quiet", "swift", "lucky", "brave", "clever", "stormy", "lazy",
	"wild", "gentle", "rusty", "frosty", "sunny", "shadow", "rowdy",
}

var animals = []string{
	"badger", "otter", "falcon", "heron", "lynx", "raven", "marten", "wombat",
	"gecko", "puffin", "jackal", "bison", "civet", "tapir", "kestrel",
}

// pick returns a uniformly-random element of list, via drawInt.
func pick(list []string) (string, error) {
	idx, err := drawInt(len(list))
	if err != nil {
		return "", err
	}
	return list[idx], nil
}
