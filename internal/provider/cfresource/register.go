package cfresource

import (
	"context"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Provider is the vendor name these types register under.
const Provider = "cloudflare"

// DriverD1 is the wire protocol a service reaches D1 through. D1 speaks
// SQLite, which is what an application connects with regardless of what
// Cloudflare runs underneath.
const DriverD1 = "sqlite"

// Resource type names, which are Cloudflare's own vocabulary rather than
// kraai's — the manifest never names these, the registry maps to them.
const (
	TypeD1Database  = "d1_database"
	TypeKVNamespace = "kv_namespace"
	TypeR2Bucket    = "r2_bucket"
	TypeQueue       = "queue"
)

// Register adds every Cloudflare storage type to reg.
//
// One function rather than four exported constructors: these are registered
// together or not at all, and a caller assembling a registry should not be
// able to wire up three of them and silently lose the fourth.
func Register(reg *resource.Registry, client *cloudflare.Client) error {
	for _, r := range Registrations(client) {
		if err := reg.Register(r); err != nil {
			return err
		}
	}
	return nil
}

// Registrations returns the Cloudflare storage registrations, exported so a
// caller can inspect or filter them before registering.
func Registrations(client *cloudflare.Client) []resource.Registration {
	return []resource.Registration{
		{
			Provider: Provider, Type: TypeD1Database,
			Capability: manifest.CapabilityDatabase,
			// The list endpoint takes a name filter, so the lookup is
			// server-side rather than a paged scan.
			Lookup: resource.LookupByAPI,
			Resource: &simple{
				provider: Provider, typ: TypeD1Database,
				// D1 speaks SQLite. A binding asking to connect over
				// Postgres must not silently receive it.
				driver: DriverD1,
				create: client.D1.Create,
				find: func(ctx context.Context, name string) (string, bool, error) {
					db, err := client.D1.FindByName(ctx, name)
					if err != nil || db == nil {
						return "", false, err
					}
					return db.UUID, true, nil
				},
				remove: client.D1.Delete,
			},
		},
		{
			Provider: Provider, Type: TypeKVNamespace,
			Capability: manifest.CapabilityKeyValue,
			// Listed and filtered on title, which the endpoint guarantees
			// unique. Paged: its default is twenty per page.
			Lookup: resource.LookupByAttr,
			Resource: &simple{
				provider: Provider, typ: TypeKVNamespace,
				create: client.KV.Create,
				find: func(ctx context.Context, name string) (string, bool, error) {
					ns, err := client.KV.FindByTitle(ctx, name)
					if err != nil || ns == nil {
						return "", false, err
					}
					return ns.ID, true, nil
				},
				remove: client.KV.Delete,
			},
		},
		{
			Provider: Provider, Type: TypeR2Bucket,
			Capability: manifest.CapabilityObjects,
			// The name is the identifier: no separate id, so no lookup step
			// before a delete.
			Lookup: resource.LookupByName,
			Resource: &simple{
				provider: Provider, typ: TypeR2Bucket,
				create: client.R2.Create,
				// A bucket's name is its id, so the lookup returns the name
				// as the identifier. Delete still addresses it by name and
				// skips this entirely; Get needs it, or a plan could not
				// tell an existing bucket from a missing one.
				find: func(ctx context.Context, name string) (string, bool, error) {
					bucket, err := client.R2.FindByName(ctx, name)
					if err != nil || bucket == nil {
						return "", false, err
					}
					return bucket.Name, true, nil
				},
				remove:       client.R2.Delete,
				deleteByName: true,
			},
		},
		{
			Provider: Provider, Type: TypeQueue,
			Capability: manifest.CapabilityQueues,
			Lookup:     resource.LookupByAttr,
			Resource: &simple{
				provider: Provider, typ: TypeQueue,
				create: client.Queues.Create,
				find: func(ctx context.Context, name string) (string, bool, error) {
					q, err := client.Queues.FindByName(ctx, name)
					if err != nil || q == nil {
						return "", false, err
					}
					return q.ID, true, nil
				},
				remove: client.Queues.Delete,
			},
		},
	}
}
