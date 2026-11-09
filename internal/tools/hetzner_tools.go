package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/blob"
	"github.com/smnhffmnn/mux/internal/config"
)

// The tools of the hetzner-cloud type. Reading tools first; the writing
// tools follow on connections that are not read-only, each checked against
// the operation's class (write or lifecycle) and the protected resources
// before a request goes out. Descriptions are generated from the registry,
// so what the tools say they take is what the spec test checks.

// TTL bounds of a record set or zone, as the document states them.
const (
	hcTTLMin = 60
	hcTTLMax = 2147483647
)

// DefaultHetznerCloudInstructions provides guidance for LLMs using the tools.
const DefaultHetznerCloudInstructions = `Hetzner Cloud project behind the Cloud API. Resources are servers, images, ISOs, volumes, networks, firewalls, floating_ips, primary_ips, load_balancers, certificates, placement_groups, ssh_keys, locations, server_types and DNS zones with their record sets (rrsets). list, get, create, update, delete and action take the resource kind as an argument; an id may be given as the id or the name.
Start with list (compact rows) and get (the full object). Server availability and prices per location come from list server_type (locations[].deprecation and prices[]), the rest of the price list from pricing. Changes to the infrastructure are asynchronous actions: the tools wait for them by default and return the action's final status; wait_action continues a wait that timed out.
DNS: a record is an rrset (name + type) with one or more records; set_records creates the rrset or replaces its records in one step. The apex is "@", names are relative to the zone.
The project's budget is 3600 requests per hour; results say when it runs low.`

// HetznerCloudInstructions composes the default instructions of a connection
// from its switches.
func HetznerCloudInstructions(conn config.Connection) string {
	parts := []string{DefaultHetznerCloudInstructions}
	switch {
	case conn.ReadOnly:
		parts = append(parts, "Read-only connection: nothing at Hetzner changes through it — no create, update, delete, action, record tools or api_request.")
	case conn.AllowLifecycle:
		parts = append(parts, "This connection may run lifecycle operations — creating servers, volumes, IPs and load balancers, deleting, resizing, rebuilding, hard power cuts, snapshots and backups, zone imports. They cost money or destroy data: name the resource and the consequence before you run one, and prefer the graceful variant (shutdown over poweroff, reboot over reset).")
	default:
		parts = append(parts, "Writing connection without lifecycle operations: it changes labels, names, reverse DNS, firewall rules, routes, attachments, protection and DNS record sets (create, set, remove, delete), and creates the free resources (networks, firewalls, SSH keys, placement groups, certificates, zones). Creating servers, volumes, IPs or load balancers, deleting a resource, resizing, rebuilding, hard power cuts, snapshots, backups and zone imports are refused until a human switches allow_lifecycle on.")
	}
	if len(conn.ProtectedResources) > 0 {
		parts = append(parts, "Protected resources ("+strings.Join(conn.ProtectedResources, ", ")+") are never changed, acted on or deleted through this connection; reading them is fine.")
	}
	return strings.Join(parts, "\n")
}

// --- Generated descriptions ---

func hcDescribeFields(fields []hcField) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		s := f.Name
		if f.Required {
			s += "*"
		}
		if f.Doc != "" {
			s += " (" + f.Doc + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func hcListDescription() string {
	var b strings.Builder
	b.WriteString("List resources of the project as compact rows (id, name, status, type, location, IPs, labels, protection …; full=true returns the objects as the API sends them). Resources: ")
	for i := range hcResources {
		r := &hcResources[i]
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(r.Kind + " — " + r.Doc)
	}
	b.WriteString(". Filters besides page/per_page by resource: ")
	for i := range hcResources {
		r := &hcResources[i]
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(r.Kind + ": ")
		parts := make([]string, 0, len(r.Filters))
		for _, f := range r.Filters {
			vals, ok := r.FilterEnums[f]
			switch {
			case ok && f == "sort":
				parts = append(parts, "sort="+strings.Join(hcSortFields(vals), "|"))
			case ok:
				parts = append(parts, f+"="+strings.Join(vals, "|"))
			default:
				parts = append(parts, f)
			}
		}
		b.WriteString(strings.Join(parts, ", "))
	}
	b.WriteString(". name is an exact match; label_selector is Hetzner's selector language (k=v, k!=v, k, !k, k in (a,b)); sort takes a listed field, optionally :asc or :desc, several comma-separated.")
	return b.String()
}

// hcSortFields reduces the document's sort enum (id, id:asc, id:desc, …) to
// the field names.
func hcSortFields(vals []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range vals {
		f, _, _ := strings.Cut(v, ":")
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func hcCreateDescription() string {
	var b strings.Builder
	b.WriteString("Create a resource. The body is the JSON object the API takes; fields by resource (* required, † the creation is a lifecycle operation — it costs money and needs allow_lifecycle): ")
	first := true
	for i := range hcResources {
		r := &hcResources[i]
		if r.Create == nil {
			continue
		}
		if !first {
			b.WriteString("; ")
		}
		first = false
		b.WriteString(r.Kind)
		if r.CreateClass == hcLifecycle {
			b.WriteString("†")
		}
		b.WriteString(": " + hcDescribeFields(r.Create))
	}
	b.WriteString(". Server types, images, locations and SSH keys may be given by name. The result carries the created object, the action(s) the API started (waited for by default) and, for a server, root_password when no SSH key was given.")
	return b.String()
}

func hcUpdateDescription() string {
	var b strings.Builder
	b.WriteString("Update a resource (PUT): fields by resource — ")
	first := true
	for i := range hcResources {
		r := &hcResources[i]
		if r.Update == nil {
			continue
		}
		if !first {
			b.WriteString("; ")
		}
		first = false
		b.WriteString(r.Kind + ": " + hcDescribeFields(r.Update))
		if len(r.UpdateLifecycle) > 0 {
			b.WriteString(" (" + strings.Join(r.UpdateLifecycle, ", ") + " is a lifecycle operation)")
		}
	}
	b.WriteString(". Labels replace the whole label set.")
	return b.String()
}

func hcActionDescription() string {
	var b strings.Builder
	b.WriteString("Run an action on a resource (POST /{collection}/{id}/actions/{action}) and wait for it. Actions by resource, params as the API takes them (* required; † lifecycle — costs money or destroys data or availability, needs allow_lifecycle): ")
	first := true
	for i := range hcResources {
		r := &hcResources[i]
		if len(r.Actions) == 0 {
			continue
		}
		if !first {
			b.WriteString("; ")
		}
		first = false
		b.WriteString(r.Kind + ": ")
		parts := make([]string, 0, len(r.Actions))
		for _, a := range r.Actions {
			s := a.Name
			if a.Class == hcLifecycle {
				s += "†"
			}
			if len(a.Body) > 0 {
				s += "{" + hcDescribeFields(a.Body) + "}"
			}
			if a.Doc != "" {
				s += " — " + a.Doc
			}
			parts = append(parts, s)
		}
		b.WriteString(strings.Join(parts, ", "))
	}
	return b.String()
}

func hcRRSetActionDescription() string {
	parts := make([]string, 0, len(hcRRSetActions))
	for _, a := range hcRRSetActions {
		s := a.Name
		if len(a.Body) > 0 {
			s += "{" + hcDescribeFields(a.Body) + "}"
		}
		parts = append(parts, s)
	}
	return "Run an action on a record set: " + strings.Join(parts, "; ") + ". Records are [{value, comment}] or plain value strings; the value identifies a record."
}

// --- Tools ---

// Tools returns the MCP tools of the connection.
func (h *HetznerCloud) Tools() []ToolDef {
	allKinds := hcKinds(nil)
	tools := []ToolDef{
		{
			Tool: mcp.NewTool("list",
				mcp.WithDescription(hcListDescription()),
				mcp.WithString("resource", mcp.Required(), mcp.Description("Resource kind"), mcp.Enum(allKinds...)),
				mcp.WithString("name", mcp.Description("Only the resource with exactly this name")),
				mcp.WithString("label_selector", mcp.Description("Label selector, e.g. env=prod or !temporary")),
				mcp.WithString("sort", mcp.Description("Sort fields, comma-separated, each optionally :asc or :desc, e.g. created:desc")),
				mcp.WithObject("filters", mcp.Description("Further filters of the resource as name → value, e.g. {\"status\": \"running\"} or {\"type\": [\"snapshot\", \"backup\"]}"), mcp.AdditionalProperties(true)),
				mcp.WithNumber("page", mcp.Description("Page, from 1 (default 1)")),
				mcp.WithNumber("per_page", mcp.Description("Rows per page, max 50 (default 25)")),
				mcp.WithBoolean("all", mcp.Description("Follow all pages (up to 2000 rows) instead of one")),
				mcp.WithBoolean("full", mcp.Description("Return the objects as the API sends them instead of compact rows")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleList,
		},
		{
			Tool: mcp.NewTool("get",
				mcp.WithDescription("One resource as the API returns it — a server with its type, image, networks, volumes and protection; a server_type with prices and availability per location; a zone with its nameservers and delegation status."),
				mcp.WithString("resource", mcp.Required(), mcp.Description("Resource kind"), mcp.Enum(allKinds...)),
				mcp.WithString("id", mcp.Required(), mcp.Description("Id or name")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleGet,
		},
		{
			Tool: mcp.NewTool("list_actions",
				mcp.WithDescription("Actions — the asynchronous tasks the API ran or runs: command, status (running, success, error), progress, started, finished, the resources concerned, the error. With resource and id: the actions of that resource; with resource alone: the project's actions on that kind — both page at 25 and list oldest first unless sorted (sort id:desc for the newest); with ids alone: exactly those actions, no paging or sorting."),
				mcp.WithString("resource", mcp.Description("Resource kind"), mcp.Enum(hcKinds(func(r *hcResource) bool { return r.ActionLists })...)),
				mcp.WithString("id", mcp.Description("Id or name of the resource (with resource)")),
				mcp.WithArray("ids", mcp.Description("Action ids (without resource)"), mcp.WithNumberItems()),
				mcp.WithArray("status", mcp.Description("Only these statuses"), mcp.WithStringEnumItems([]string{"running", "success", "error"})),
				mcp.WithString("sort", mcp.Description("id, command, status, started or finished, each optionally :asc or :desc; comma-separated")),
				mcp.WithNumber("page", mcp.Description("Page, from 1")),
				mcp.WithNumber("per_page", mcp.Description("Rows per page, max 50")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleListActions,
		},
		{
			Tool: mcp.NewTool("get_action",
				mcp.WithDescription("One action by id, with its error when it failed."),
				mcp.WithNumber("id", mcp.Required(), mcp.Description("Action id")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleGetAction,
		},
		{
			Tool: mcp.NewTool("wait_action",
				mcp.WithDescription("Wait for actions to finish (status success or error), polling the API every few seconds up to the timeout. Use it when a create, delete or action tool returned a running action."),
				mcp.WithArray("ids", mcp.Required(), mcp.Description("Action ids"), mcp.WithNumberItems()),
				mcp.WithNumber("timeout", mcp.Description("Seconds to wait (default 60, max 600)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleWaitAction,
		},
		{
			Tool: mcp.NewTool("metrics",
				mcp.WithDescription("Time series of a server (cpu, disk, network) or a load_balancer (open_connections, connections_per_second, requests_per_second, bandwidth) between start and end, as the API returns them (values per step in seconds)."),
				mcp.WithString("resource", mcp.Required(), mcp.Description("server or load_balancer"), mcp.Enum(hcKinds(func(r *hcResource) bool { return r.Metrics != nil })...)),
				mcp.WithString("id", mcp.Required(), mcp.Description("Id or name")),
				mcp.WithArray("type", mcp.Required(), mcp.Description("Metric types"), mcp.WithStringItems()),
				mcp.WithString("start", mcp.Required(), mcp.Description("RFC 3339, e.g. 2026-11-08T00:00:00Z")),
				mcp.WithString("end", mcp.Required(), mcp.Description("RFC 3339")),
				mcp.WithNumber("step", mcp.Description("Resolution in seconds (the API picks one when empty)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleMetrics,
		},
		{
			Tool: mcp.NewTool("pricing",
				mcp.WithDescription("The project's price list: currency, VAT rate, Primary and Floating IPs, images (snapshots per GB), volumes per GB, backup surcharge, server types and load balancer types per location (hourly and monthly, net and gross, included traffic). server_types narrows the list to those names, location to one location."),
				mcp.WithArray("server_types", mcp.Description("Only these server type names, e.g. [\"cx23\", \"cax11\"]"), mcp.WithStringItems()),
				mcp.WithString("location", mcp.Description("Only prices of this location, e.g. fsn1")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handlePricing,
		},
		{
			Tool: mcp.NewTool("list_rrsets",
				mcp.WithDescription("Record sets of a DNS zone: name (relative to the zone, @ for the apex), type, ttl, records (value, comment), labels, protection. The zone's own SOA and NS sets are among them."),
				mcp.WithString("zone", mcp.Required(), mcp.Description("Zone id or name (the domain)")),
				mcp.WithString("name", mcp.Description("Only record sets with this name")),
				mcp.WithArray("type", mcp.Description("Only these record types"), mcp.WithStringEnumItems(hcRRSetTypes)),
				mcp.WithString("label_selector", mcp.Description("Label selector")),
				mcp.WithString("sort", mcp.Description("id, name or created, optionally :desc")),
				mcp.WithNumber("page", mcp.Description("Page, from 1")),
				mcp.WithNumber("per_page", mcp.Description("Rows per page, max 50")),
				mcp.WithBoolean("all", mcp.Description("Follow all pages")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleListRRSets,
		},
		{
			Tool: mcp.NewTool("get_rrset",
				mcp.WithDescription("One record set of a zone by name and type."),
				mcp.WithString("zone", mcp.Required(), mcp.Description("Zone id or name")),
				mcp.WithString("name", mcp.Required(), mcp.Description("Record set name, relative to the zone; @ for the apex")),
				mcp.WithString("type", mcp.Required(), mcp.Description("Record type"), mcp.Enum(hcRRSetTypes...)),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleGetRRSet,
		},
		{
			Tool: mcp.NewTool("zonefile",
				mcp.WithDescription("The zone as a BIND zone file (RFC 1034/1035), generated by Hetzner — the complete export of a primary zone."),
				mcp.WithString("zone", mcp.Required(), mcp.Description("Zone id or name")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleZonefile,
		},
		{
			Tool: mcp.NewTool("api_get",
				mcp.WithDescription("Raw GET against the Cloud API — any /v1/… path including the query string, for what no tool shapes (network members, a resource's own action list with filters, new API surface). The JSON comes back as Hetzner sends it. Paths are written plain (no percent-encoding, no . or .. segments)."),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path starting with /v1/, query string allowed")),
				mcp.WithString("output_file", mcp.Description("Save the response instead of returning it inline: \"blob\" or an absolute path")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: h.handleAPIGet,
		},
	}
	if h.readOnly {
		return tools
	}
	return append(tools,
		ToolDef{
			Tool: mcp.NewTool("create",
				mcp.WithDescription(hcCreateDescription()),
				mcp.WithString("resource", mcp.Required(), mcp.Description("Resource kind"), mcp.Enum(hcKinds(func(r *hcResource) bool { return r.Create != nil })...)),
				mcp.WithObject("body", mcp.Required(), mcp.Description("The request body as the API takes it"), mcp.AdditionalProperties(true)),
				mcp.WithBoolean("wait", mcp.Description("Wait for the API's actions to finish (default true)")),
				mcp.WithNumber("timeout", mcp.Description("Seconds to wait (default 60, max 600)")),
			),
			Handler: h.handleCreate,
		},
		ToolDef{
			Tool: mcp.NewTool("update",
				mcp.WithDescription(hcUpdateDescription()),
				mcp.WithString("resource", mcp.Required(), mcp.Description("Resource kind"), mcp.Enum(hcKinds(func(r *hcResource) bool { return r.Update != nil })...)),
				mcp.WithString("id", mcp.Required(), mcp.Description("Id or name")),
				mcp.WithObject("body", mcp.Required(), mcp.Description("The fields to set"), mcp.AdditionalProperties(true)),
			),
			Handler: h.handleUpdate,
		},
		ToolDef{
			Tool: mcp.NewTool("delete",
				mcp.WithDescription("Delete a resource — a lifecycle operation (needs allow_lifecycle): servers, volumes, images, networks, firewalls, IPs, load balancers, certificates, placement groups, SSH keys and zones go away with their data; Hetzner's own delete protection is checked by the API. Servers, load balancers and zones answer with an action that is waited for."),
				mcp.WithString("resource", mcp.Required(), mcp.Description("Resource kind"), mcp.Enum(hcKinds(func(r *hcResource) bool { return r.Deletable })...)),
				mcp.WithString("id", mcp.Required(), mcp.Description("Id or name")),
				mcp.WithBoolean("wait", mcp.Description("Wait for the delete action where there is one (default true)")),
				mcp.WithNumber("timeout", mcp.Description("Seconds to wait (default 60, max 600)")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: h.handleDelete,
		},
		ToolDef{
			Tool: mcp.NewTool("action",
				mcp.WithDescription(hcActionDescription()),
				mcp.WithString("resource", mcp.Required(), mcp.Description("Resource kind"), mcp.Enum(hcKinds(func(r *hcResource) bool { return len(r.Actions) > 0 })...)),
				mcp.WithString("id", mcp.Required(), mcp.Description("Id or name of the resource")),
				mcp.WithString("action", mcp.Required(), mcp.Description("Action name, e.g. poweron, reboot, attach, set_rules, change_ttl")),
				mcp.WithObject("params", mcp.Description("The action's body as the API takes it"), mcp.AdditionalProperties(true)),
				mcp.WithBoolean("wait", mcp.Description("Wait for the action to finish (default true)")),
				mcp.WithNumber("timeout", mcp.Description("Seconds to wait (default 60, max 600)")),
			),
			Handler: h.handleAction,
		},
		ToolDef{
			Tool: mcp.NewTool("set_records",
				mcp.WithDescription("Set the records of a DNS record set in one step: creates the record set (name + type) when the zone has none, otherwise replaces its records; ttl is set when given. The usual way to point a name at an address."),
				mcp.WithString("zone", mcp.Required(), mcp.Description("Zone id or name")),
				mcp.WithString("name", mcp.Required(), mcp.Description("Record set name, relative to the zone; @ for the apex; lower case")),
				mcp.WithString("type", mcp.Required(), mcp.Description("Record type"), mcp.Enum(hcRRSetTypes...)),
				mcp.WithArray("records", mcp.Required(), mcp.Description("Record values (\"198.51.100.1\") or objects {value, comment}; TXT values quoted as in a zone file"), mcp.Items(hcRecordItems())),
				mcp.WithNumber("ttl", mcp.Description("TTL in seconds (60…2147483647); omitted: the zone's default for a new set, unchanged for an existing one")),
				mcp.WithBoolean("wait", mcp.Description("Wait for the action (default true)")),
				mcp.WithNumber("timeout", mcp.Description("Seconds to wait (default 60, max 600)")),
			),
			Handler: h.handleSetRecords,
		},
		ToolDef{
			Tool: mcp.NewTool("create_rrset",
				mcp.WithDescription("Create a DNS record set in a zone. Fails when a set of that name and type exists — set_records covers both cases."),
				mcp.WithString("zone", mcp.Required(), mcp.Description("Zone id or name")),
				mcp.WithString("name", mcp.Required(), mcp.Description("Record set name, relative to the zone; @ for the apex")),
				mcp.WithString("type", mcp.Required(), mcp.Description("Record type"), mcp.Enum(hcRRSetTypes...)),
				mcp.WithArray("records", mcp.Required(), mcp.Description("Record values or objects {value, comment}"), mcp.Items(hcRecordItems())),
				mcp.WithNumber("ttl", mcp.Description("TTL in seconds; omitted: the zone's default")),
				mcp.WithObject("labels", mcp.Description("Labels"), mcp.AdditionalProperties(true)),
				mcp.WithBoolean("wait", mcp.Description("Wait for the action (default true)")),
				mcp.WithNumber("timeout", mcp.Description("Seconds to wait (default 60, max 600)")),
			),
			Handler: h.handleCreateRRSet,
		},
		ToolDef{
			Tool: mcp.NewTool("update_rrset",
				mcp.WithDescription("Update the labels of a record set (PUT); records and TTL stay — the TTL changes through rrset_action change_ttl or set_records."),
				mcp.WithString("zone", mcp.Required(), mcp.Description("Zone id or name")),
				mcp.WithString("name", mcp.Required(), mcp.Description("Record set name")),
				mcp.WithString("type", mcp.Required(), mcp.Description("Record type"), mcp.Enum(hcRRSetTypes...)),
				mcp.WithObject("body", mcp.Required(), mcp.Description("{labels: {key: value}}"), mcp.AdditionalProperties(true)),
			),
			Handler: h.handleUpdateRRSet,
		},
		ToolDef{
			Tool: mcp.NewTool("delete_rrset",
				mcp.WithDescription("Delete a record set with all its records. Hetzner's change protection on the set is checked by the API."),
				mcp.WithString("zone", mcp.Required(), mcp.Description("Zone id or name")),
				mcp.WithString("name", mcp.Required(), mcp.Description("Record set name")),
				mcp.WithString("type", mcp.Required(), mcp.Description("Record type"), mcp.Enum(hcRRSetTypes...)),
				mcp.WithBoolean("wait", mcp.Description("Wait for the action (default true)")),
				mcp.WithNumber("timeout", mcp.Description("Seconds to wait (default 60, max 600)")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: h.handleDeleteRRSet,
		},
		ToolDef{
			Tool: mcp.NewTool("rrset_action",
				mcp.WithDescription(hcRRSetActionDescription()),
				mcp.WithString("zone", mcp.Required(), mcp.Description("Zone id or name")),
				mcp.WithString("name", mcp.Required(), mcp.Description("Record set name")),
				mcp.WithString("type", mcp.Required(), mcp.Description("Record type"), mcp.Enum(hcRRSetTypes...)),
				mcp.WithString("action", mcp.Required(), mcp.Description("Action name"), mcp.Enum(hcRRSetActionNames()...)),
				mcp.WithObject("params", mcp.Description("The action's body"), mcp.AdditionalProperties(true)),
				mcp.WithBoolean("wait", mcp.Description("Wait for the action (default true)")),
				mcp.WithNumber("timeout", mcp.Description("Seconds to wait (default 60, max 600)")),
			),
			Handler: h.handleRRSetAction,
		},
		ToolDef{
			Tool: mcp.NewTool("import_zonefile",
				mcp.WithDescription("Import a BIND zone file into a zone, replacing every record set of the zone — a lifecycle operation (needs allow_lifecycle). Record sets with Hetzner's change protection make the import fail."),
				mcp.WithString("zone", mcp.Required(), mcp.Description("Zone id or name")),
				mcp.WithString("zonefile", mcp.Required(), mcp.Description("The zone file text")),
				mcp.WithBoolean("wait", mcp.Description("Wait for the action (default true)")),
				mcp.WithNumber("timeout", mcp.Description("Seconds to wait (default 60, max 600)")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: h.handleImportZonefile,
		},
		ToolDef{
			Tool: mcp.NewTool("api_request",
				mcp.WithDescription("Raw POST, PUT or DELETE against the Cloud API with a JSON body, for what no tool shapes. The path is classified against the same registry as the tools: a write of a known kind needs what the tool would need (a delete, a create of a billed resource or a lifecycle action needs allow_lifecycle), a protected resource is refused, and a write the registry does not know is admitted only with allow_lifecycle. The response comes back as Hetzner sends it; actions are not waited for (wait_action does that)."),
				mcp.WithString("method", mcp.Required(), mcp.Description("POST, PUT or DELETE"), mcp.Enum("POST", "PUT", "DELETE")),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path starting with /v1/")),
				mcp.WithObject("body", mcp.Description("JSON body"), mcp.AdditionalProperties(true)),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: h.handleAPIRequest,
		},
	)
}

// hcRecordItems is the schema of a records argument: a value string or a
// {value, comment} object.
func hcRecordItems() map[string]any {
	return map[string]any{"anyOf": []any{
		map[string]any{"type": "string", "description": "the record value"},
		map[string]any{"type": "object", "properties": map[string]any{
			"value":   map[string]any{"type": "string"},
			"comment": map[string]any{"type": "string"},
		}, "required": []string{"value"}, "additionalProperties": false},
	}}
}

func hcRRSetActionNames() []string {
	out := make([]string, 0, len(hcRRSetActions))
	for _, a := range hcRRSetActions {
		out = append(out, a.Name)
	}
	return out
}

// hetznerCloudConditionalToolNames are the tools whose presence depends on
// the configuration (read_only); they are removed before every registration
// so a reload that switches read_only on leaves none behind.
func hetznerCloudConditionalToolNames(connName string) []string {
	always := map[string]bool{}
	for _, td := range (&HetznerCloud{readOnly: true}).Tools() {
		always[td.Tool.Name] = true
	}
	var names []string
	for _, td := range (&HetznerCloud{}).Tools() {
		if !always[td.Tool.Name] {
			names = append(names, config.SanitizeToolName(connName+"_"+td.Tool.Name))
		}
	}
	return names
}

// --- Results ---

// result renders v as indented JSON, with the rate-limit note when the
// budget runs low. A result above hcInlineMax goes to a blob when there is a
// store; otherwise it is cut with a note.
func (h *HetznerCloud) result(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	text := string(data)
	if len(data) > hcInlineMax {
		if info, err := storeBytesAsBlob(data, blob.Meta{Name: h.name + "-result.json", ContentType: "application/json", Source: "hetzner-cloud"}); err == nil {
			text = string(data[:hcInlineMax]) + fmt.Sprintf("\n\n[result is %d bytes — the first %d inline, the whole result in the blob below; narrow the call with filters, per_page or without full]\n%s", len(data), hcInlineMax, blobReference(info))
		} else {
			text = string(data[:hcInlineMax]) + fmt.Sprintf("\n\n[truncated at %d of %d bytes — narrow the call with filters, per_page or without full]", hcInlineMax, len(data))
		}
	}
	if note := h.rateNote(); note != "" {
		text += "\n\n" + note
	}
	return mcp.NewToolResultText(text), nil
}

func hcErr(err error) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(err.Error()), nil
}

// --- Argument helpers ---

func hcResourceArg(req mcp.CallToolRequest, pred func(*hcResource) bool) (*hcResource, error) {
	kind := strings.TrimSpace(req.GetString("resource", ""))
	res := hcResourceByKind(kind)
	if res == nil {
		return nil, fmt.Errorf("resource %q is not one of %s", kind, strings.Join(hcKinds(nil), ", "))
	}
	if pred != nil && !pred(res) {
		return nil, fmt.Errorf("resource %q does not support this operation", kind)
	}
	return res, nil
}

func hcObjectArg(req mcp.CallToolRequest, key string, required bool) (map[string]any, error) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		if required {
			return nil, fmt.Errorf("%s is required and must be a JSON object", key)
		}
		return map[string]any{}, nil
	}
	switch x := v.(type) {
	case map[string]any:
		return x, nil
	case string:
		// A client that serializes objects as strings is met halfway.
		var m map[string]any
		if err := json.Unmarshal([]byte(x), &m); err != nil || m == nil {
			return nil, fmt.Errorf("%s must be a JSON object", key)
		}
		return m, nil
	}
	return nil, fmt.Errorf("%s must be a JSON object, got %T", key, v)
}

// hcIDArg reads the id argument as a string whether the client sent a
// string or a number.
func hcIDArg(req mcp.CallToolRequest) string {
	v, ok := req.GetArguments()["id"]
	if !ok || v == nil {
		return ""
	}
	return hcScalar(v)
}

func hcWaitArgs(req mcp.CallToolRequest) (bool, time.Duration) {
	wait := true
	if v, ok := req.GetArguments()["wait"].(bool); ok {
		wait = v
	}
	timeout := hcWaitDefault
	if v, ok := req.GetArguments()["timeout"].(float64); ok && v > 0 {
		if v > hcWaitMax.Seconds() {
			v = hcWaitMax.Seconds()
		}
		timeout = time.Duration(v * float64(time.Second))
	}
	return wait, timeout
}

func hcPageArgs(req mcp.CallToolRequest, q url.Values) {
	if v, ok := req.GetArguments()["page"].(float64); ok && v >= 1 {
		q.Set("page", strconv.Itoa(int(min(v, 1e9))))
	}
	if v, ok := req.GetArguments()["per_page"].(float64); ok && v >= 1 {
		q.Set("per_page", strconv.Itoa(int(min(v, hcPerPageMax))))
	}
}

// hcAddSort splits a comma-separated sort argument into repeated parameters.
func hcAddSort(q url.Values, sortArg string) {
	for _, s := range strings.Split(sortArg, ",") {
		if s = strings.TrimSpace(s); s != "" {
			q.Add("sort", s)
		}
	}
}

func hcStringList(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, hcScalar(e))
		}
		return out
	case nil:
		return nil
	default:
		return []string{hcScalar(x)}
	}
}

func hcScalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func hcNumberList(v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("ids must be an array of numbers")
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s := hcScalar(e)
		if !hcNumeric(s) {
			return nil, fmt.Errorf("id %q is not a number", s)
		}
		out = append(out, s)
	}
	return out, nil
}

// hcRecords normalizes a records argument: plain strings become {value}.
func hcRecords(v any) ([]map[string]any, error) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("records must be a non-empty array of values or {value, comment} objects")
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		switch x := e.(type) {
		case string:
			if strings.TrimSpace(x) == "" {
				return nil, fmt.Errorf("a record value is empty")
			}
			out = append(out, map[string]any{"value": x})
		case map[string]any:
			if s, _ := x["value"].(string); strings.TrimSpace(s) == "" {
				return nil, fmt.Errorf("a record object lacks its value")
			}
			rec := map[string]any{"value": x["value"]}
			if c, ok := x["comment"]; ok && c != nil {
				rec["comment"] = c
			}
			out = append(out, rec)
		default:
			return nil, fmt.Errorf("a record must be a value string or {value, comment}, got %T", e)
		}
	}
	return out, nil
}

// hcRRSetArgs reads zone, name and type and builds the record set path.
func (h *HetznerCloud) hcRRSetArgs(req mcp.CallToolRequest) (zone, name, typ, path string, err error) {
	zone = strings.TrimSpace(req.GetString("zone", ""))
	name = strings.TrimSpace(req.GetString("name", ""))
	typ = strings.ToUpper(strings.TrimSpace(req.GetString("type", "")))
	if zone == "" || name == "" || typ == "" {
		return "", "", "", "", fmt.Errorf("zone, name and type are required")
	}
	if err := hcPathValue(zone, "zone"); err != nil {
		return "", "", "", "", err
	}
	if err := hcPathValue(name, "record set name"); err != nil {
		return "", "", "", "", err
	}
	ok := false
	for _, t := range hcRRSetTypes {
		if t == typ {
			ok = true
		}
	}
	if !ok {
		return "", "", "", "", fmt.Errorf("type %q is not one of %s", typ, strings.Join(hcRRSetTypes, ", "))
	}
	path = "/zones/" + url.PathEscape(zone) + "/rrsets/" + url.PathEscape(name) + "/" + typ
	return zone, name, typ, path, nil
}

// --- Reading handlers ---

func (h *HetznerCloud) handleList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := hcResourceArg(req, nil)
	if err != nil {
		return hcErr(err)
	}
	q := url.Values{}
	allowed := map[string]bool{}
	for _, f := range res.Filters {
		allowed[f] = true
	}
	for _, key := range []string{"name", "label_selector"} {
		if v := strings.TrimSpace(req.GetString(key, "")); v != "" {
			if !allowed[key] {
				return hcErr(fmt.Errorf("%s has no %s filter (filters: %s)", res.Kind, key, strings.Join(res.Filters, ", ")))
			}
			q.Set(key, v)
		}
	}
	if v := req.GetString("sort", ""); strings.TrimSpace(v) != "" {
		if !allowed["sort"] {
			return hcErr(fmt.Errorf("%s cannot be sorted (filters: %s)", res.Kind, strings.Join(res.Filters, ", ")))
		}
		hcAddSort(q, v)
	}
	filters, err := hcObjectArg(req, "filters", false)
	if err != nil {
		return hcErr(err)
	}
	for k, v := range filters {
		if !allowed[k] {
			return hcErr(fmt.Errorf("%s has no %q filter (filters: %s)", res.Kind, k, strings.Join(res.Filters, ", ")))
		}
		if k == "sort" {
			for _, s := range hcStringList(v) {
				hcAddSort(q, s)
			}
			continue
		}
		for _, s := range hcStringList(v) {
			q.Add(k, s)
		}
	}
	full := req.GetBool("full", false)
	out := map[string]any{"resource": res.Kind}
	var items []map[string]any
	if req.GetBool("all", false) {
		got, total, complete, err := h.listAll(ctx, "/"+res.Collection, res.Collection, q)
		if err != nil {
			return hcErr(err)
		}
		items = got
		if total != nil {
			out["total_entries"] = *total
		}
		if !complete {
			out["note"] = fmt.Sprintf("stopped after %d pages of %d — narrow the listing with filters", hcMaxPages, hcPerPageMax)
		}
	} else {
		hcPageArgs(req, q)
		got, pg, err := h.listPage(ctx, "/"+res.Collection, res.Collection, q)
		if err != nil {
			return hcErr(err)
		}
		items = got
		if pg != nil {
			out["page"] = pg.Page
			out["per_page"] = pg.PerPage
			if pg.TotalEntries != nil {
				out["total_entries"] = *pg.TotalEntries
			}
			if pg.NextPage != nil {
				out["next_page"] = *pg.NextPage
			}
			if pg.LastPage != nil {
				out["last_page"] = *pg.LastPage
			}
		}
	}
	out["count"] = len(items)
	if full {
		out[res.Collection] = items
	} else {
		rows := make([]map[string]any, 0, len(items))
		for _, it := range items {
			rows = append(rows, hcSummarize(it, res.Summary))
		}
		out[res.Collection] = rows
	}
	return h.result(out)
}

func (h *HetznerCloud) handleGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := hcResourceArg(req, nil)
	if err != nil {
		return hcErr(err)
	}
	id, obj, err := h.resolve(ctx, res, hcIDArg(req))
	if err != nil {
		return hcErr(err)
	}
	if obj != nil && res.IDParam == "id" {
		// The lookup by name returned the listing's object, which the API
		// renders like the item; one read saved.
		return h.result(map[string]any{res.Item: obj})
	}
	var raw map[string]any
	if _, err := h.callJSON(ctx, http.MethodGet, "/"+res.Collection+"/"+url.PathEscape(id), nil, nil, &raw); err != nil {
		return hcErr(err)
	}
	return h.result(raw)
}

func (h *HetznerCloud) handleListActions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	kind := strings.TrimSpace(req.GetString("resource", ""))
	idArg := strings.TrimSpace(hcIDArg(req))
	idsArg, hasIDs := req.GetArguments()["ids"]
	hasIDs = hasIDs && idsArg != nil
	if kind == "" && !hasIDs {
		return hcErr(fmt.Errorf("give a resource (and optionally its id) or action ids — the project-wide list takes ids only"))
	}
	if kind == "" {
		for _, k := range []string{"status", "sort", "page", "per_page", "id"} {
			if v, ok := req.GetArguments()[k]; ok && v != nil && hcScalar(v) != "" {
				return hcErr(fmt.Errorf("%s has no effect with ids alone — the project-wide GET /actions takes ids and nothing else; give a resource to filter, sort or page", k))
			}
		}
	}
	if kind != "" && idArg != "" && hasIDs {
		return hcErr(fmt.Errorf("ids cannot be combined with a resource id — one resource's actions are listed by resource and id, specific actions by ids"))
	}
	q := url.Values{}
	path := "/actions"
	if kind != "" {
		res := hcResourceByKind(kind)
		if res == nil || !res.ActionLists {
			return hcErr(fmt.Errorf("resource %q has no action list", kind))
		}
		if idArg != "" {
			id, _, err := h.resolve(ctx, res, idArg)
			if err != nil {
				return hcErr(err)
			}
			path = "/" + res.Collection + "/" + url.PathEscape(id) + "/actions"
		} else {
			path = "/" + res.Collection + "/actions"
		}
		// The per-kind and per-resource lists filter, sort and page; the
		// project-wide GET /actions takes ids and nothing else.
		for _, st := range hcStringList(req.GetArguments()["status"]) {
			q.Add("status", st)
		}
		if v := req.GetString("sort", ""); strings.TrimSpace(v) != "" {
			hcAddSort(q, v)
		}
		hcPageArgs(req, q)
	}
	if hasIDs {
		list, err := hcNumberList(idsArg)
		if err != nil {
			return hcErr(err)
		}
		if len(list) == 0 {
			return hcErr(fmt.Errorf("ids is empty"))
		}
		q["id"] = list
	}
	items, pg, err := h.listPage(ctx, path, "actions", q)
	if err != nil {
		return hcErr(err)
	}
	rows := make([]map[string]any, 0, len(items))
	for _, a := range items {
		rows = append(rows, hcActionSummary(a))
	}
	out := map[string]any{"count": len(rows), "actions": rows}
	if pg != nil {
		out["page"] = pg.Page
		if pg.TotalEntries != nil {
			out["total_entries"] = *pg.TotalEntries
		}
		if pg.NextPage != nil {
			out["next_page"] = *pg.NextPage
		}
	}
	return h.result(out)
}

func (h *HetznerCloud) handleGetAction(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := strings.TrimSpace(hcIDArg(req))
	if !hcNumeric(id) {
		return hcErr(fmt.Errorf("id (an action id) is required"))
	}
	var raw map[string]any
	if _, err := h.callJSON(ctx, http.MethodGet, "/actions/"+id, nil, nil, &raw); err != nil {
		return hcErr(err)
	}
	return h.result(raw)
}

func (h *HetznerCloud) handleWaitAction(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ids, err := hcNumberList(req.GetArguments()["ids"])
	if err != nil || len(ids) == 0 {
		return hcErr(fmt.Errorf("ids (a non-empty array of action ids) is required"))
	}
	ids = hcUnique(ids)
	_, timeout := hcWaitArgs(req)
	var actions []map[string]any
	for start := 0; start < len(ids); start += hcActionIDsChunk {
		chunk := ids[start:min(start+hcActionIDsChunk, len(ids))]
		items, _, err := h.listPage(ctx, "/actions", "actions", url.Values{"id": chunk})
		if err != nil {
			return hcErr(err)
		}
		actions = append(actions, items...)
	}
	if len(actions) != len(ids) {
		return hcErr(fmt.Errorf("%d of %d actions found — unknown ids: %s", len(actions), len(ids), strings.Join(hcMissingIDs(ids, actions), ", ")))
	}
	return h.actionResult(ctx, actions, map[string]any{}, true, timeout)
}

// hcSplitActionsOnly is hcSplitActions for callers that keep their own extras.
func hcSplitActionsOnly(data []byte) ([]map[string]any, error) {
	actions, _, err := hcSplitActions(data)
	return actions, err
}

func hcUnique(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func hcMissingIDs(ids []string, actions []map[string]any) []string {
	seen := map[string]bool{}
	for _, a := range actions {
		seen[hcID(a)] = true
	}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out
}

func (h *HetznerCloud) handleMetrics(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := hcResourceArg(req, func(r *hcResource) bool { return r.Metrics != nil })
	if err != nil {
		return hcErr(err)
	}
	id, _, err := h.resolve(ctx, res, hcIDArg(req))
	if err != nil {
		return hcErr(err)
	}
	types := hcStringList(req.GetArguments()["type"])
	if len(types) == 0 {
		return hcErr(fmt.Errorf("type is required: one or more of %s", strings.Join(res.Metrics, ", ")))
	}
	q := url.Values{}
	for _, t := range types {
		known := false
		for _, m := range res.Metrics {
			if m == t {
				known = true
			}
		}
		if !known {
			return hcErr(fmt.Errorf("%s has no metric %q (one of %s)", res.Kind, t, strings.Join(res.Metrics, ", ")))
		}
		q.Add("type", t)
	}
	start, end := strings.TrimSpace(req.GetString("start", "")), strings.TrimSpace(req.GetString("end", ""))
	if start == "" || end == "" {
		return hcErr(fmt.Errorf("start and end (RFC 3339) are required"))
	}
	q.Set("start", start)
	q.Set("end", end)
	if v, ok := req.GetArguments()["step"].(float64); ok && v > 0 {
		q.Set("step", strconv.Itoa(int(v)))
	}
	var raw map[string]any
	if _, err := h.callJSON(ctx, http.MethodGet, "/"+res.Collection+"/"+url.PathEscape(id)+"/metrics", q, nil, &raw); err != nil {
		return hcErr(err)
	}
	return h.result(raw)
}

func (h *HetznerCloud) handlePricing(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var raw struct {
		Pricing map[string]any `json:"pricing"`
	}
	if _, err := h.callJSON(ctx, http.MethodGet, "/pricing", nil, nil, &raw); err != nil {
		return hcErr(err)
	}
	if raw.Pricing == nil {
		return hcErr(fmt.Errorf("the response carries no pricing"))
	}
	wanted := map[string]bool{}
	for _, s := range hcStringList(req.GetArguments()["server_types"]) {
		wanted[strings.ToLower(strings.TrimSpace(s))] = true
	}
	location := strings.TrimSpace(req.GetString("location", ""))
	if len(wanted) == 0 && location == "" {
		return h.result(map[string]any{"pricing": raw.Pricing})
	}
	// server_types narrows the server type list; location narrows every list
	// whose entries carry prices per location (server types, load balancer
	// types, Primary IPs, …) to that location's entries.
	notes := []string{}
	for key, v := range raw.Pricing {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		var kept []any
		for _, e := range list {
			m, _ := e.(map[string]any)
			if m == nil {
				kept = append(kept, e)
				continue
			}
			if key == "server_types" && len(wanted) > 0 && !wanted[strings.ToLower(hcName(m))] {
				continue
			}
			if prices, ok := m["prices"].([]any); ok && location != "" {
				var at []any
				for _, p := range prices {
					pm, _ := p.(map[string]any)
					if pm != nil && strings.EqualFold(hcScalar(pm["location"]), location) {
						at = append(at, p)
					}
				}
				if len(at) == 0 {
					continue
				}
				cp := make(map[string]any, len(m))
				for k, val := range m {
					cp[k] = val
				}
				cp["prices"] = at
				m = cp
			}
			kept = append(kept, m)
		}
		if kept == nil {
			kept = []any{}
			if len(list) > 0 {
				notes = append(notes, fmt.Sprintf("%s: nothing matches the filter", key))
			}
		}
		raw.Pricing[key] = kept
	}
	out := map[string]any{"pricing": raw.Pricing}
	if len(notes) > 0 {
		sort.Strings(notes)
		out["note"] = strings.Join(notes, "; ")
	}
	return h.result(out)
}

func (h *HetznerCloud) handleListRRSets(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	zone := strings.TrimSpace(req.GetString("zone", ""))
	if err := hcPathValue(zone, "zone"); err != nil {
		return hcErr(err)
	}
	q := url.Values{}
	if v := strings.TrimSpace(req.GetString("name", "")); v != "" {
		q.Set("name", v)
	}
	for _, t := range hcStringList(req.GetArguments()["type"]) {
		q.Add("type", strings.ToUpper(t))
	}
	if v := strings.TrimSpace(req.GetString("label_selector", "")); v != "" {
		q.Set("label_selector", v)
	}
	if v := req.GetString("sort", ""); strings.TrimSpace(v) != "" {
		hcAddSort(q, v)
	}
	path := "/zones/" + url.PathEscape(zone) + "/rrsets"
	out := map[string]any{"zone": zone}
	var items []map[string]any
	if req.GetBool("all", false) {
		got, total, complete, err := h.listAll(ctx, path, "rrsets", q)
		if err != nil {
			return hcErr(err)
		}
		items = got
		if total != nil {
			out["total_entries"] = *total
		}
		if !complete {
			out["note"] = fmt.Sprintf("stopped after %d pages", hcMaxPages)
		}
	} else {
		hcPageArgs(req, q)
		got, pg, err := h.listPage(ctx, path, "rrsets", q)
		if err != nil {
			return hcErr(err)
		}
		items = got
		if pg != nil {
			out["page"] = pg.Page
			if pg.TotalEntries != nil {
				out["total_entries"] = *pg.TotalEntries
			}
			if pg.NextPage != nil {
				out["next_page"] = *pg.NextPage
			}
		}
	}
	rows := make([]map[string]any, 0, len(items))
	for _, it := range items {
		delete(it, "zone") // the zone's id, repeated on every row
		rows = append(rows, it)
	}
	out["count"] = len(rows)
	out["rrsets"] = rows
	return h.result(out)
}

func (h *HetznerCloud) handleGetRRSet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	_, _, _, path, err := h.hcRRSetArgs(req)
	if err != nil {
		return hcErr(err)
	}
	var raw map[string]any
	if _, err := h.callJSON(ctx, http.MethodGet, path, nil, nil, &raw); err != nil {
		return hcErr(err)
	}
	return h.result(raw)
}

func (h *HetznerCloud) handleZonefile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	zone := strings.TrimSpace(req.GetString("zone", ""))
	if err := hcPathValue(zone, "zone"); err != nil {
		return hcErr(err)
	}
	var raw struct {
		Zonefile string `json:"zonefile"`
	}
	if _, err := h.callJSON(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone)+"/zonefile", nil, nil, &raw); err != nil {
		return hcErr(err)
	}
	text := raw.Zonefile
	if note := h.rateNote(); note != "" {
		text += "\n; " + note
	}
	return mcp.NewToolResultText(text), nil
}

func (h *HetznerCloud) handleAPIGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := hcAPIPath(req.GetString("path", ""))
	if err != nil {
		return hcErr(err)
	}
	outputFile := req.GetString("output_file", "")
	if outputFile != "" {
		if _, err := validateOutputFile(outputFile); err != nil {
			return hcErr(err)
		}
	}
	return h.raw(ctx, http.MethodGet, path, nil, outputFile)
}

// raw performs a pass-through call and renders the response as it came.
func (h *HetznerCloud) raw(ctx context.Context, method, pathWithQuery string, body json.RawMessage, outputFile string) (*mcp.CallToolResult, error) {
	path, query, _ := strings.Cut(pathWithQuery, "?")
	q, err := url.ParseQuery(query)
	if err != nil {
		return hcErr(fmt.Errorf("query string: %w", err))
	}
	var payload any
	if body != nil {
		payload = body
	}
	resp, err := h.do(ctx, method, strings.TrimPrefix(path, hcVersionPath), q, payload)
	if err != nil {
		return hcErr(err)
	}
	if outputFile != "" && resp.Status < 400 {
		hr := &http.Response{StatusCode: resp.Status, Header: resp.Header, Body: io.NopCloser(strings.NewReader(string(resp.Body)))}
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		return saveResponseToFile(hr, ct, outputFile)
	}
	hr := &http.Response{StatusCode: resp.Status, Header: resp.Header, Body: io.NopCloser(strings.NewReader(string(resp.Body)))}
	res, err := rawHTTPResult(hr, hcRawInlineMax, "pass output_file to save the whole response")
	if err == nil && res != nil && !res.IsError {
		if note := h.rateNote(); note != "" {
			res.Content = append(res.Content, mcp.NewTextContent(note))
		}
	}
	return res, err
}

// --- Writing handlers ---

// actionResult waits for the actions when asked and renders them with the
// extras of the response.
func (h *HetznerCloud) actionResult(ctx context.Context, actions []map[string]any, extra map[string]any, wait bool, timeout time.Duration) (*mcp.CallToolResult, error) {
	out := map[string]any{}
	for k, v := range extra {
		out[k] = v
	}
	done := true
	var notes []string
	waitFailed := false
	if wait && len(actions) > 0 {
		var err error
		actions, done, err = h.waitActions(ctx, actions, timeout)
		if err != nil {
			waitFailed = true
			notes = append(notes, "waiting for the actions failed: "+err.Error())
		}
	}
	rows := make([]map[string]any, 0, len(actions))
	failed := 0
	for _, a := range actions {
		rows = append(rows, hcActionSummary(a))
		if hcActionStatus(a) == "error" {
			failed++
		}
	}
	out["actions"] = rows
	if !done {
		ids := make([]string, 0, len(actions))
		for _, a := range actions {
			if hcActionStatus(a) == "running" {
				ids = append(ids, hcID(a))
			}
		}
		if waitFailed {
			notes = append(notes, fmt.Sprintf("the actions may still be running — continue with wait_action ids [%s]", strings.Join(ids, ", ")))
		} else {
			notes = append(notes, fmt.Sprintf("still running after %s — continue with wait_action ids [%s]", timeout.Round(time.Second), strings.Join(ids, ", ")))
		}
	}
	if prior, ok := out["note"].(string); ok && prior != "" {
		notes = append([]string{prior}, notes...)
	}
	if len(notes) > 0 {
		out["note"] = strings.Join(notes, "; ")
	}
	if failed > 0 {
		out["failed"] = failed
	}
	res, rerr := h.result(out)
	if failed > 0 && res != nil {
		res.IsError = true
	}
	return res, rerr
}

func (h *HetznerCloud) handleCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := hcResourceArg(req, func(r *hcResource) bool { return r.Create != nil })
	if err != nil {
		return hcErr(err)
	}
	body, err := hcObjectArg(req, "body", true)
	if err != nil {
		return hcErr(err)
	}
	for _, f := range res.Create {
		if f.Required {
			if _, ok := body[f.Name]; !ok {
				return hcErr(fmt.Errorf("creating a %s needs %s in the body (%s)", res.Kind, f.Name, hcDescribeFields(res.Create)))
			}
		}
	}
	if err := h.permit(res.CreateClass, "creating a "+res.Kind); err != nil {
		return hcErr(err)
	}
	wait, timeout := hcWaitArgs(req)
	resp, err := h.do(ctx, http.MethodPost, "/"+res.Collection, nil, body)
	if err != nil {
		return hcErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return hcErr(h.errorOf(resp))
	}
	actions, extra, err := hcSplitActions(resp.Body)
	if err != nil {
		return hcErr(err)
	}
	extra["created"] = res.Kind
	return h.actionResult(ctx, actions, extra, wait, timeout)
}

func (h *HetznerCloud) handleUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := hcResourceArg(req, func(r *hcResource) bool { return r.Update != nil })
	if err != nil {
		return hcErr(err)
	}
	body, err := hcObjectArg(req, "body", true)
	if err != nil {
		return hcErr(err)
	}
	if len(body) == 0 {
		return hcErr(fmt.Errorf("body is empty — fields: %s", hcDescribeFields(res.Update)))
	}
	class := hcWrite
	if hcHasLifecycleKey(res, body) {
		class = hcLifecycle
	}
	if err := h.permit(class, "updating a "+res.Kind); err != nil {
		return hcErr(err)
	}
	id, obj, err := h.resolve(ctx, res, hcIDArg(req))
	if err != nil {
		return hcErr(err)
	}
	if err := h.protect(ctx, res, id, obj); err != nil {
		return hcErr(err)
	}
	var raw map[string]any
	if _, err := h.callJSON(ctx, http.MethodPut, "/"+res.Collection+"/"+url.PathEscape(id), nil, body, &raw); err != nil {
		return hcErr(err)
	}
	return h.result(raw)
}

func (h *HetznerCloud) handleDelete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := hcResourceArg(req, func(r *hcResource) bool { return r.Deletable })
	if err != nil {
		return hcErr(err)
	}
	if err := h.permit(hcLifecycle, "deleting a "+res.Kind); err != nil {
		return hcErr(err)
	}
	id, obj, err := h.resolve(ctx, res, hcIDArg(req))
	if err != nil {
		return hcErr(err)
	}
	if err := h.protect(ctx, res, id, obj); err != nil {
		return hcErr(err)
	}
	wait, timeout := hcWaitArgs(req)
	resp, err := h.do(ctx, http.MethodDelete, "/"+res.Collection+"/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return hcErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return hcErr(h.errorOf(resp))
	}
	actions, extra, err := hcSplitActions(resp.Body)
	if err != nil {
		return hcErr(err)
	}
	extra["deleted"] = res.Kind
	extra["id"] = id
	if obj != nil {
		extra["name"] = hcName(obj)
	}
	return h.actionResult(ctx, actions, extra, wait, timeout)
}

func (h *HetznerCloud) handleAction(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := hcResourceArg(req, func(r *hcResource) bool { return len(r.Actions) > 0 })
	if err != nil {
		return hcErr(err)
	}
	name := strings.TrimSpace(req.GetString("action", ""))
	act := res.action(name)
	if act == nil {
		names := make([]string, 0, len(res.Actions))
		for _, a := range res.Actions {
			names = append(names, a.Name)
		}
		return hcErr(fmt.Errorf("%s has no action %q (one of %s)", res.Kind, name, strings.Join(names, ", ")))
	}
	params, err := hcObjectArg(req, "params", false)
	if err != nil {
		return hcErr(err)
	}
	for _, f := range act.Body {
		if f.Required {
			if _, ok := params[f.Name]; !ok {
				return hcErr(fmt.Errorf("%s %s needs %s in params (%s)", res.Kind, act.Name, f.Name, hcDescribeFields(act.Body)))
			}
		}
	}
	if err := h.permit(act.Class, fmt.Sprintf("%s on a %s", act.Name, res.Kind)); err != nil {
		return hcErr(err)
	}
	id, obj, err := h.resolve(ctx, res, hcIDArg(req))
	if err != nil {
		return hcErr(err)
	}
	if err := h.protect(ctx, res, id, obj); err != nil {
		return hcErr(err)
	}
	wait, timeout := hcWaitArgs(req)
	var body any
	if len(params) > 0 || len(act.Body) > 0 {
		body = params
	}
	resp, err := h.do(ctx, http.MethodPost, "/"+res.Collection+"/"+url.PathEscape(id)+"/actions/"+act.Name, nil, body)
	if err != nil {
		return hcErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return hcErr(h.errorOf(resp))
	}
	actions, extra, err := hcSplitActions(resp.Body)
	if err != nil {
		return hcErr(err)
	}
	return h.actionResult(ctx, actions, extra, wait, timeout)
}

// zoneGuard checks a write on a zone or its record sets: the class, then
// the protected resources (the zone's name).
func (h *HetznerCloud) zoneGuard(ctx context.Context, class hcClass, what, zone string) error {
	if err := h.permit(class, what); err != nil {
		return err
	}
	return h.protect(ctx, hcResourceByKind("zone"), zone, nil)
}

func (h *HetznerCloud) handleSetRecords(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	zone, name, typ, path, err := h.hcRRSetArgs(req)
	if err != nil {
		return hcErr(err)
	}
	records, err := hcRecords(req.GetArguments()["records"])
	if err != nil {
		return hcErr(err)
	}
	if err := h.zoneGuard(ctx, hcWrite, "setting records in zone "+zone, zone); err != nil {
		return hcErr(err)
	}
	wait, timeout := hcWaitArgs(req)
	ttl, hasTTL := req.GetArguments()["ttl"].(float64)
	if hasTTL && (ttl < hcTTLMin || ttl > hcTTLMax || ttl != float64(int64(ttl))) {
		return hcErr(fmt.Errorf("ttl must be a whole number of seconds between %d and %d", hcTTLMin, hcTTLMax))
	}
	var existing map[string]map[string]any
	resp, err := h.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return hcErr(err)
	}
	switch {
	case resp.Status == http.StatusNotFound:
		body := map[string]any{"name": name, "type": typ, "records": records}
		if hasTTL {
			body["ttl"] = int(ttl)
		}
		created, err := h.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone)+"/rrsets", nil, body)
		if err != nil {
			return hcErr(err)
		}
		if created.Status < 200 || created.Status >= 300 {
			return hcErr(h.errorOf(created))
		}
		actions, extra, err := hcSplitActions(created.Body)
		if err != nil {
			return hcErr(err)
		}
		extra["created"] = "rrset"
		return h.actionResult(ctx, actions, extra, wait, timeout)
	case resp.Status < 200 || resp.Status >= 300:
		return hcErr(h.errorOf(resp))
	}
	if err := json.Unmarshal(resp.Body, &existing); err != nil {
		return hcErr(fmt.Errorf("decode Hetzner response: %w", err))
	}
	set, err := h.do(ctx, http.MethodPost, path+"/actions/set_records", nil, map[string]any{"records": records})
	if err != nil {
		return hcErr(err)
	}
	if set.Status < 200 || set.Status >= 300 {
		return hcErr(h.errorOf(set))
	}
	actions, extra, err := hcSplitActions(set.Body)
	if err != nil {
		return hcErr(err)
	}
	extra["replaced"] = "rrset"
	extra["rrset"] = map[string]any{"name": name, "type": typ, "records": records}
	if hasTTL {
		if cur, _ := existing["rrset"]["ttl"].(float64); cur != ttl {
			// The records are replaced by now: a TTL change that fails is
			// reported next to the set_records action, not instead of it.
			ttlResp, err := h.do(ctx, http.MethodPost, path+"/actions/change_ttl", nil, map[string]any{"ttl": int(ttl)})
			var ttlErr error
			switch {
			case err != nil:
				ttlErr = err
			case ttlResp.Status < 200 || ttlResp.Status >= 300:
				ttlErr = h.errorOf(ttlResp)
			default:
				more, splitErr := hcSplitActionsOnly(ttlResp.Body)
				if splitErr != nil {
					ttlErr = splitErr
				}
				actions = append(actions, more...)
			}
			if ttlErr != nil {
				extra["note"] = "the records were replaced; the TTL change failed and may not have been applied — read the record set (get_rrset) before repeating it: " + ttlErr.Error()
				res, rerr := h.actionResult(ctx, actions, extra, wait, timeout)
				if res != nil {
					res.IsError = true
				}
				return res, rerr
			}
		}
	}
	return h.actionResult(ctx, actions, extra, wait, timeout)
}

func (h *HetznerCloud) handleCreateRRSet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	zone, name, typ, _, err := h.hcRRSetArgs(req)
	if err != nil {
		return hcErr(err)
	}
	records, err := hcRecords(req.GetArguments()["records"])
	if err != nil {
		return hcErr(err)
	}
	if err := h.zoneGuard(ctx, hcWrite, "creating a record set in zone "+zone, zone); err != nil {
		return hcErr(err)
	}
	body := map[string]any{"name": name, "type": typ, "records": records}
	if ttl, ok := req.GetArguments()["ttl"].(float64); ok {
		if ttl < hcTTLMin || ttl > hcTTLMax || ttl != float64(int64(ttl)) {
			return hcErr(fmt.Errorf("ttl must be a whole number of seconds between %d and %d", hcTTLMin, hcTTLMax))
		}
		body["ttl"] = int(ttl)
	}
	if labels, ok := req.GetArguments()["labels"].(map[string]any); ok && len(labels) > 0 {
		body["labels"] = labels
	}
	wait, timeout := hcWaitArgs(req)
	resp, err := h.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone)+"/rrsets", nil, body)
	if err != nil {
		return hcErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return hcErr(h.errorOf(resp))
	}
	actions, extra, err := hcSplitActions(resp.Body)
	if err != nil {
		return hcErr(err)
	}
	extra["created"] = "rrset"
	return h.actionResult(ctx, actions, extra, wait, timeout)
}

func (h *HetznerCloud) handleUpdateRRSet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	zone, _, _, path, err := h.hcRRSetArgs(req)
	if err != nil {
		return hcErr(err)
	}
	body, err := hcObjectArg(req, "body", true)
	if err != nil {
		return hcErr(err)
	}
	if err := h.zoneGuard(ctx, hcWrite, "updating a record set in zone "+zone, zone); err != nil {
		return hcErr(err)
	}
	var raw map[string]any
	if _, err := h.callJSON(ctx, http.MethodPut, path, nil, body, &raw); err != nil {
		return hcErr(err)
	}
	return h.result(raw)
}

func (h *HetznerCloud) handleDeleteRRSet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	zone, name, typ, path, err := h.hcRRSetArgs(req)
	if err != nil {
		return hcErr(err)
	}
	if err := h.zoneGuard(ctx, hcWrite, "deleting a record set in zone "+zone, zone); err != nil {
		return hcErr(err)
	}
	wait, timeout := hcWaitArgs(req)
	resp, err := h.do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return hcErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return hcErr(h.errorOf(resp))
	}
	actions, extra, err := hcSplitActions(resp.Body)
	if err != nil {
		return hcErr(err)
	}
	extra["deleted"] = "rrset"
	extra["rrset"] = name + "/" + typ
	return h.actionResult(ctx, actions, extra, wait, timeout)
}

func (h *HetznerCloud) handleRRSetAction(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	zone, _, _, path, err := h.hcRRSetArgs(req)
	if err != nil {
		return hcErr(err)
	}
	name := strings.TrimSpace(req.GetString("action", ""))
	act := hcRRSetAction(name)
	if act == nil {
		return hcErr(fmt.Errorf("record sets have no action %q (one of %s)", name, strings.Join(hcRRSetActionNames(), ", ")))
	}
	params, err := hcObjectArg(req, "params", false)
	if err != nil {
		return hcErr(err)
	}
	if recs, ok := params["records"]; ok {
		normalized, err := hcRecords(recs)
		if err != nil {
			return hcErr(err)
		}
		if act.Name == "update_records" {
			for _, r := range normalized {
				if _, ok := r["comment"]; !ok {
					return hcErr(fmt.Errorf("update_records sets comments: every record needs {value, comment}"))
				}
			}
		}
		params["records"] = normalized
	}
	for _, f := range act.Body {
		if f.Required {
			if _, ok := params[f.Name]; !ok {
				return hcErr(fmt.Errorf("%s needs %s in params (%s)", act.Name, f.Name, hcDescribeFields(act.Body)))
			}
		}
	}
	if err := h.zoneGuard(ctx, act.Class, act.Name+" on a record set in zone "+zone, zone); err != nil {
		return hcErr(err)
	}
	wait, timeout := hcWaitArgs(req)
	resp, err := h.do(ctx, http.MethodPost, path+"/actions/"+act.Name, nil, params)
	if err != nil {
		return hcErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return hcErr(h.errorOf(resp))
	}
	actions, extra, err := hcSplitActions(resp.Body)
	if err != nil {
		return hcErr(err)
	}
	return h.actionResult(ctx, actions, extra, wait, timeout)
}

func (h *HetznerCloud) handleImportZonefile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	zone := strings.TrimSpace(req.GetString("zone", ""))
	zonefile := req.GetString("zonefile", "")
	if err := hcPathValue(zone, "zone"); err != nil {
		return hcErr(err)
	}
	if strings.TrimSpace(zonefile) == "" {
		return hcErr(fmt.Errorf("zonefile is required"))
	}
	if err := h.zoneGuard(ctx, hcLifecycle, "importing a zone file into zone "+zone, zone); err != nil {
		return hcErr(err)
	}
	wait, timeout := hcWaitArgs(req)
	resp, err := h.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone)+"/actions/import_zonefile", nil, map[string]any{"zonefile": zonefile})
	if err != nil {
		return hcErr(err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return hcErr(h.errorOf(resp))
	}
	actions, extra, err := hcSplitActions(resp.Body)
	if err != nil {
		return hcErr(err)
	}
	extra["imported"] = zone
	return h.actionResult(ctx, actions, extra, wait, timeout)
}

func (h *HetznerCloud) handleAPIRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	method := strings.ToUpper(strings.TrimSpace(req.GetString("method", "")))
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete:
	default:
		return hcErr(fmt.Errorf("method must be POST, PUT or DELETE (api_get reads)"))
	}
	path, err := hcAPIPath(req.GetString("path", ""))
	if err != nil {
		return hcErr(err)
	}
	if strings.Contains(path, "?") {
		return hcErr(fmt.Errorf("a writing request takes no query string"))
	}
	var body json.RawMessage
	var bodyObj map[string]any
	if v, ok := req.GetArguments()["body"]; ok && v != nil {
		obj, err := hcObjectArg(req, "body", true)
		if err != nil {
			return hcErr(err)
		}
		bodyObj = obj
		body, _ = json.Marshal(obj)
	}
	target := hcClassify(method, path, bodyObj)
	if !target.Known {
		if err := h.permit(hcLifecycle, target.What); err != nil {
			return hcErr(fmt.Errorf("%s is not in this build's registry of the API; %w", target.What, err))
		}
		if len(h.guards) > 0 && (target.Res == nil || target.ID == "") {
			// A write mux cannot place on a resource cannot be held against
			// the protected list — with guards set, it is refused.
			return hcErr(fmt.Errorf("%s is not in this build's registry of the API and names no resource mux can check against protected_resources — refused on a connection with protected resources; use the tools or an exact /v1/{collection}/{id}… path", target.What))
		}
	} else if err := h.permit(target.Class, target.What); err != nil {
		return hcErr(err)
	}
	if target.Res != nil && target.ID != "" {
		if err := h.protect(ctx, target.Res, target.ID, nil); err != nil {
			return hcErr(err)
		}
	}
	return h.raw(ctx, method, path, body, "")
}

// --- Verification (desktop Test button) ---

// HetznerCloudInfo is what Verify learned about the project.
type HetznerCloudInfo struct {
	Servers       int
	Zones         int
	RateRemaining int
	RateLimit     int
}

// Verify reads the project the way the tools do: one page of servers and
// one of zones. A wrong or revoked token fails here with the API's message;
// the counts tell which project a valid token belongs to.
func (h *HetznerCloud) Verify(ctx context.Context) (*HetznerCloudInfo, error) {
	info := &HetznerCloudInfo{}
	for _, coll := range []string{"servers", "zones"} {
		_, pg, err := h.listPage(ctx, "/"+coll, coll, url.Values{"per_page": {"1"}})
		if err != nil {
			return nil, fmt.Errorf("GET /%s: %w", coll, err)
		}
		n := 0
		if pg != nil && pg.TotalEntries != nil {
			n = *pg.TotalEntries
		}
		if coll == "servers" {
			info.Servers = n
		} else {
			info.Zones = n
		}
	}
	h.mu.Lock()
	info.RateRemaining, info.RateLimit = h.rateRemaining, h.rateLimit
	h.mu.Unlock()
	return info, nil
}
