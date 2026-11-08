package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

// The hetzner-cloud connection type puts one Hetzner Cloud project behind
// the Cloud API (https://docs.hetzner.cloud/reference/cloud): servers,
// volumes, networks, firewalls, Floating and Primary IPs, Load Balancers,
// certificates, placement groups, SSH keys, images, ISOs, locations, server
// types, pricing, actions — and the DNS zones with their record sets that
// the API carries since the DNS console moved into the Cloud Console.
//
// The API is regular: every resource has a collection, an item, actions
// under /{collection}/{id}/actions/{name}, the same pagination, the same
// error envelope and the same asynchronous Action object. The type
// therefore describes the resources in a registry (hcResources) and offers
// a small set of tools that take the resource as an argument — list, get,
// create, update, delete, action — next to the DNS record tools, the action
// and metrics readers, pricing and a confined pass-through. The registry is
// also the contract the spec tests hold against Hetzner's published OpenAPI
// document (hetzner_spec_test.go): every path, query parameter, body field
// and summary field the type names must be in the document, and every
// operation of the document must be reachable through the registry.
//
// Three guards live in code, not in instructions: read_only keeps the
// reading tools only; allow_lifecycle admits what costs money or destroys
// data or availability (creating billed resources, every delete, resize,
// change of type, rebuild, hard power cut, backups and snapshots, zone
// import); protected_resources names what the connection never changes,
// acts on or deletes, whatever the other two say. The pass-through
// (api_request) is classified by method and path against the same registry
// and refused where a tool would be.

const (
	hcRequestTimeout = 60 * time.Second
	hcMaxBody        = 16 << 20  // largest response read
	hcPerPageDefault = 25        // the API's default page size
	hcPerPageMax     = 50        // the API's cap
	hcInlineMax      = 256 << 10 // JSON results inlined up to this size; larger ones go to a blob
	hcRawInlineMax   = 1 << 20   // pass-through responses inlined up to this size
	hcWaitDefault    = 60 * time.Second
	hcWaitMax        = 10 * time.Minute
	hcVersionPath    = "/v1"
	hcRateLimitWarn  = 360 // results mention the remaining request budget below this (10 % of the default 3600/h)
	hcActionIDsChunk = 25  // ids per GET /actions poll (the API takes many; hcloud-go uses 25)
)

// hcMaxPages is where all=true stops (40 × 50 items); a variable so tests
// can shorten the walk.
var hcMaxPages = 40

// hcPollIntervals paces the action polling: quick at first, then every
// five seconds. The API's budget refills by one request per second.
var hcPollIntervals = []time.Duration{time.Second, time.Second, 2 * time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second}

// --- Operation classes ---

// hcClass is the class of an operation, the unit the guards work on.
type hcClass uint8

const (
	hcRead      hcClass = iota // always allowed
	hcWrite                    // not on read_only connections: reversible changes without cost — labels, names, DNS records, rules, attachments, protection, graceful reboots
	hcLifecycle                // additionally needs allow_lifecycle: costs money, destroys data, or cuts power hard (graceful shutdown and reboot are writes)
)

func (c hcClass) String() string {
	switch c {
	case hcRead:
		return "read"
	case hcWrite:
		return "write"
	default:
		return "lifecycle"
	}
}

// --- Registry ---

// hcField is a body field the type documents and the spec test checks.
type hcField struct {
	Name     string
	Required bool
	Doc      string
}

func hcF(name string, required bool, doc string) hcField { return hcField{name, required, doc} }

// hcAction is one POST /{collection}/{id}/actions/{Name}.
type hcAction struct {
	Name  string
	Class hcClass
	Body  []hcField
	Doc   string
	Extra []string // top-level response keys beside action (root_password, image, wss_url, password)
}

// hcResource describes one resource of the API: paths, filters, body
// fields, actions and the fields its compact listing shows.
type hcResource struct {
	Kind       string // as the tools name it: "server"
	Collection string // the API collection: "servers"
	Item       string // wrapper of a single object in responses: "server"
	IDParam    string // the path parameter of the item: "id", or "id_or_name" for zones
	Doc        string

	Filters     []string            // query parameters of the list call besides page and per_page
	FilterEnums map[string][]string // documented values of a filter, for the description

	Create          []hcField // nil: not creatable through the API
	CreateClass     hcClass
	CreateExtra     []string // top-level response keys beside the item (action, next_actions, root_password)
	Update          []hcField
	UpdateLifecycle []string // update body keys that turn the update into a lifecycle operation
	Deletable       bool
	DeleteAction    bool // the delete answers with an action (servers, load balancers, zones) instead of 204
	Actions         []hcAction
	ActionLists     bool     // GET /{c}/actions, GET /{c}/actions/{id}, GET /{c}/{id}/actions exist
	Metrics         []string // metric types of GET /{c}/{id}/metrics, nil when there is none

	// Summary lists the fields of the compact listing as paths into the
	// object ("name", "server_type.name"), optionally with an alias
	// ("ipv4=public_net.ipv4.ip"). Checked against the item schema by the
	// spec test.
	Summary []string
}

var (
	hcFieldName        = hcF("name", false, "")
	hcFieldLabels      = hcF("labels", false, "key/value labels")
	hcNameLabels       = []hcField{hcFieldName, hcFieldLabels}
	hcSortNameCreated  = []string{"id", "id:asc", "id:desc", "name", "name:asc", "name:desc", "created", "created:asc", "created:desc"}
	hcProtectionDelete = hcAction{Name: "change_protection", Class: hcWrite, Body: []hcField{hcF("delete", false, "")}, Doc: "Hetzner's own delete protection on or off"}
	hcDNSPtr           = hcAction{Name: "change_dns_ptr", Class: hcWrite, Body: []hcField{hcF("ip", true, ""), hcF("dns_ptr", true, "null resets to the default")}, Doc: "reverse DNS of one of the resource's IPs"}
)

// hcResources is the registry — in the order the descriptions list them.
var hcResources = []hcResource{
	{
		Kind: "server", Collection: "servers", Item: "server", IDParam: "id",
		Doc:     "virtual machines",
		Filters: []string{"name", "label_selector", "sort", "status"},
		FilterEnums: map[string][]string{
			"status": {"running", "initializing", "starting", "stopping", "off", "deleting", "migrating", "rebuilding", "unknown"},
			"sort":   hcSortNameCreated,
		},
		Create: []hcField{
			hcF("name", true, "a hostname, unique in the project"),
			hcF("server_type", true, "id or name, e.g. cx23"),
			hcF("image", true, "id or name, e.g. debian-12"),
			hcF("location", false, "id or name, e.g. fsn1, nbg1, hel1"),
			hcF("ssh_keys", false, "ids or names"),
			hcF("user_data", false, "cloud-init"),
			hcF("labels", false, ""),
			hcF("start_after_create", false, "default true"),
			hcF("volumes", false, "ids to attach"),
			hcF("networks", false, "ids to attach to"),
			hcF("firewalls", false, "[{\"firewall\": id}]"),
			hcF("placement_group", false, "id"),
			hcF("automount", false, ""),
			hcF("public_net", false, "{enable_ipv4, enable_ipv6, ipv4, ipv6}"),
		},
		CreateClass: hcLifecycle,
		CreateExtra: []string{"action", "next_actions", "root_password"},
		Update:      hcNameLabels,
		Deletable:   true, DeleteAction: true,
		Actions: []hcAction{
			{Name: "poweron", Class: hcWrite},
			{Name: "reboot", Class: hcWrite, Doc: "graceful (ACPI)"},
			{Name: "shutdown", Class: hcWrite, Doc: "graceful (ACPI), the server stays off and billed"},
			{Name: "poweroff", Class: hcLifecycle, Doc: "hard power cut"},
			{Name: "reset", Class: hcLifecycle, Doc: "hard reset"},
			{Name: "rebuild", Class: hcLifecycle, Body: []hcField{hcF("image", true, "id or name"), hcF("user_data", false, "")}, Doc: "wipes the disk", Extra: []string{"root_password"}},
			{Name: "change_type", Class: hcLifecycle, Body: []hcField{hcF("server_type", true, "id or name"), hcF("upgrade_disk", true, "false keeps the disk size so a downgrade stays possible")}, Doc: "server must be off"},
			{Name: "create_image", Class: hcLifecycle, Body: []hcField{hcF("type", false, "snapshot (default) or backup"), hcF("description", false, ""), hcF("labels", false, "")}, Doc: "snapshots are billed per GB", Extra: []string{"image"}},
			{Name: "enable_backup", Class: hcLifecycle, Doc: "+20 % of the server price"},
			{Name: "disable_backup", Class: hcLifecycle, Doc: "deletes the existing backups"},
			{Name: "enable_rescue", Class: hcWrite, Body: []hcField{hcF("type", false, "linux64"), hcF("ssh_keys", false, "ids")}, Doc: "next boot into the rescue system", Extra: []string{"root_password"}},
			{Name: "disable_rescue", Class: hcWrite},
			{Name: "attach_iso", Class: hcWrite, Body: []hcField{hcF("iso", true, "id or name")}},
			{Name: "detach_iso", Class: hcWrite},
			{Name: "reset_password", Class: hcWrite, Extra: []string{"root_password"}},
			{Name: "request_console", Class: hcWrite, Doc: "VNC console URL and password", Extra: []string{"wss_url", "password"}},
			{Name: "attach_to_network", Class: hcWrite, Body: []hcField{hcF("network", true, "id"), hcF("ip", false, ""), hcF("alias_ips", false, ""), hcF("ip_range", false, "")}},
			{Name: "detach_from_network", Class: hcWrite, Body: []hcField{hcF("network", true, "id")}},
			{Name: "change_alias_ips", Class: hcWrite, Body: []hcField{hcF("network", true, "id"), hcF("alias_ips", true, "")}},
			hcDNSPtr,
			{Name: "change_protection", Class: hcWrite, Body: []hcField{hcF("delete", false, ""), hcF("rebuild", false, "")}, Doc: "Hetzner's own delete and rebuild protection"},
			{Name: "add_to_placement_group", Class: hcWrite, Body: []hcField{hcF("placement_group", true, "id")}, Doc: "server must be off"},
			{Name: "remove_from_placement_group", Class: hcWrite},
		},
		ActionLists: true,
		Metrics:     []string{"cpu", "disk", "network"},
		Summary: []string{"id", "name", "status", "server_type=server_type.name", "location=location.name", "ipv4=public_net.ipv4.ip", "ipv6=public_net.ipv6.ip",
			"private_net", "image=image.name", "volumes", "firewalls=public_net.firewalls", "placement_group=placement_group.id", "protection", "labels", "locked", "rescue_enabled", "backup_window", "created"},
	},
	{
		Kind: "image", Collection: "images", Item: "image", IDParam: "id",
		Doc:     "system images, apps, snapshots and backups",
		Filters: []string{"sort", "type", "status", "bound_to", "include_deprecated", "name", "label_selector", "architecture"},
		FilterEnums: map[string][]string{
			"type": {"system", "app", "snapshot", "backup"}, "status": {"available", "creating", "unavailable"}, "architecture": {"x86", "arm"}, "sort": hcSortNameCreated,
		},
		Update:          []hcField{hcF("description", false, ""), hcF("type", false, "snapshot — converts a backup into a snapshot, which is billed"), hcFieldLabels},
		UpdateLifecycle: []string{"type"},
		Deletable:       true,
		Actions:         []hcAction{hcProtectionDelete},
		ActionLists:     true,
		Summary: []string{"id", "name", "description", "type", "status", "os_flavor", "os_version", "architecture", "image_size", "disk_size", "created_from", "bound_to",
			"deprecation", "protection", "labels", "created"},
	},
	{
		Kind: "iso", Collection: "isos", Item: "iso", IDParam: "id",
		Doc:         "installation media to attach to a server",
		Filters:     []string{"name", "architecture", "include_architecture_wildcard"},
		FilterEnums: map[string][]string{"architecture": {"x86", "arm"}},
		Summary:     []string{"id", "name", "description", "type", "architecture", "deprecation"},
	},
	{
		Kind: "volume", Collection: "volumes", Item: "volume", IDParam: "id",
		Doc:         "block storage, billed per GB",
		Filters:     []string{"status", "sort", "name", "label_selector"},
		FilterEnums: map[string][]string{"status": {"available", "creating"}, "sort": hcSortNameCreated},
		Create: []hcField{
			hcF("name", true, ""), hcF("size", true, "GB, 10 to 10240"),
			hcF("location", false, "id or name; or assignee"),
			hcF("assignee", false, "{\"id\": server id, \"type\": \"server\"} — created in the server's location and attached"),
			hcF("format", false, "xfs or ext4"), hcF("automount", false, ""), hcF("labels", false, ""),
		},
		CreateClass: hcLifecycle,
		CreateExtra: []string{"action", "next_actions"},
		Update:      hcNameLabels,
		Deletable:   true,
		Actions: []hcAction{
			{Name: "attach", Class: hcWrite, Body: []hcField{hcF("assignee", true, "{\"id\": server id, \"type\": \"server\"}"), hcF("automount", false, "")}},
			{Name: "detach", Class: hcWrite},
			{Name: "resize", Class: hcLifecycle, Body: []hcField{hcF("size", true, "GB, only larger")}},
			hcProtectionDelete,
		},
		ActionLists: true,
		Summary:     []string{"id", "name", "size", "status", "assignee", "location=location.name", "linux_device", "format", "protection", "labels", "created"},
	},
	{
		Kind: "network", Collection: "networks", Item: "network", IDParam: "id",
		Doc:         "private networks with subnets and routes",
		Filters:     []string{"sort", "name", "label_selector"},
		FilterEnums: map[string][]string{"sort": hcSortNameCreated},
		Create: []hcField{
			hcF("name", true, ""), hcF("ip_range", true, "e.g. 10.0.0.0/16"),
			hcF("subnets", false, "[{type: cloud|server|vswitch, ip_range, network_zone, vswitch_id}]"),
			hcF("routes", false, "[{destination, gateway}]"), hcF("expose_routes_to_vswitch", false, ""), hcF("labels", false, ""),
		},
		CreateClass: hcWrite,
		Update:      []hcField{hcFieldName, hcFieldLabels, hcF("expose_routes_to_vswitch", false, "")},
		Deletable:   true,
		Actions: []hcAction{
			{Name: "add_subnet", Class: hcWrite, Body: []hcField{hcF("type", true, "cloud, server or vswitch"), hcF("network_zone", true, "e.g. eu-central"), hcF("ip_range", false, ""), hcF("vswitch_id", false, "")}},
			{Name: "delete_subnet", Class: hcLifecycle, Body: []hcField{hcF("ip_range", true, "")}, Doc: "the subnet's addresses go away"},
			{Name: "add_route", Class: hcWrite, Body: []hcField{hcF("destination", true, ""), hcF("gateway", true, "")}},
			{Name: "delete_route", Class: hcWrite, Body: []hcField{hcF("destination", true, ""), hcF("gateway", true, "")}},
			{Name: "change_ip_range", Class: hcWrite, Body: []hcField{hcF("ip_range", true, "only a larger range")}},
			hcProtectionDelete,
		},
		ActionLists: true,
		Summary:     []string{"id", "name", "ip_range", "subnets", "routes", "servers", "load_balancers", "expose_routes_to_vswitch", "protection", "labels", "created"},
	},
	{
		Kind: "firewall", Collection: "firewalls", Item: "firewall", IDParam: "id",
		Doc:         "stateful packet filters applied to servers",
		Filters:     []string{"sort", "name", "label_selector"},
		FilterEnums: map[string][]string{"sort": hcSortNameCreated},
		Create: []hcField{
			hcF("name", true, ""),
			hcF("rules", false, "[{direction: in|out, protocol: tcp|udp|icmp|esp|gre, port: \"22\" or \"1024-5000\" (tcp and udp only), source_ips: [cidr…] for in, destination_ips for out, description}]"),
			hcF("apply_to", false, "[{type: server, server: {id}} | {type: label_selector, label_selector: {selector}}]"),
			hcF("labels", false, ""),
		},
		CreateClass: hcWrite,
		CreateExtra: []string{"actions"},
		Update:      hcNameLabels,
		Deletable:   true,
		Actions: []hcAction{
			{Name: "set_rules", Class: hcWrite, Body: []hcField{hcF("rules", true, "replaces all rules; [] removes them")}, Doc: "a wrong rule set can lock you out — reversible with another set_rules"},
			{Name: "apply_to_resources", Class: hcWrite, Body: []hcField{hcF("apply_to", true, "")}},
			{Name: "remove_from_resources", Class: hcWrite, Body: []hcField{hcF("remove_from", true, "")}},
		},
		ActionLists: true,
		Summary:     []string{"id", "name", "rules", "applied_to", "labels", "created"},
	},
	{
		Kind: "floating_ip", Collection: "floating_ips", Item: "floating_ip", IDParam: "id",
		Doc:         "IPs that move between servers, billed monthly",
		Filters:     []string{"name", "label_selector", "sort"},
		FilterEnums: map[string][]string{"sort": {"id", "id:asc", "id:desc", "created", "created:asc", "created:desc"}},
		Create: []hcField{
			hcF("type", true, "ipv4 or ipv6"), hcF("name", false, ""), hcF("home_location", false, "id or name, unless server is given"),
			hcF("server", false, "id to assign to"), hcF("description", false, ""), hcF("labels", false, ""),
		},
		CreateClass: hcLifecycle,
		CreateExtra: []string{"action"},
		Update:      []hcField{hcFieldName, hcF("description", false, ""), hcFieldLabels},
		Deletable:   true,
		Actions: []hcAction{
			{Name: "assign", Class: hcWrite, Body: []hcField{hcF("server", true, "id")}},
			{Name: "unassign", Class: hcWrite},
			hcDNSPtr,
			{Name: "change_protection", Class: hcWrite, Body: []hcField{hcF("delete", true, "")}, Doc: "Hetzner's own delete protection on or off"},
		},
		ActionLists: true,
		Summary:     []string{"id", "name", "ip", "type", "server", "home_location=home_location.name", "dns_ptr", "blocked", "description", "protection", "labels", "created"},
	},
	{
		Kind: "primary_ip", Collection: "primary_ips", Item: "primary_ip", IDParam: "id",
		Doc:         "the public IPs of servers, billed monthly for IPv4",
		Filters:     []string{"name", "label_selector", "ip", "sort"},
		FilterEnums: map[string][]string{"sort": {"id", "id:asc", "id:desc", "created", "created:asc", "created:desc"}},
		Create: []hcField{
			hcF("name", true, ""), hcF("type", true, "ipv4 or ipv6"), hcF("location", false, "id or name, unless assignee_id is given"),
			hcF("assignee_type", false, "server or unassigned"), hcF("assignee_id", false, "server id"), hcF("auto_delete", false, "delete with the server"), hcF("labels", false, ""),
		},
		CreateClass: hcLifecycle,
		CreateExtra: []string{"action"},
		Update:      []hcField{hcFieldName, hcFieldLabels, hcF("auto_delete", false, "")},
		Deletable:   true,
		Actions: []hcAction{
			{Name: "assign", Class: hcWrite, Body: []hcField{hcF("assignee_type", true, "server"), hcF("assignee_id", true, "server id")}, Doc: "server must be off"},
			{Name: "unassign", Class: hcWrite, Doc: "server must be off"},
			hcDNSPtr,
			{Name: "change_protection", Class: hcWrite, Body: []hcField{hcF("delete", true, "")}, Doc: "Hetzner's own delete protection on or off"},
		},
		ActionLists: true,
		Summary:     []string{"id", "name", "ip", "type", "assignee_type", "assignee_id", "location=location.name", "dns_ptr", "auto_delete", "blocked", "protection", "labels", "created"},
	},
	{
		Kind: "load_balancer", Collection: "load_balancers", Item: "load_balancer", IDParam: "id",
		Doc:         "managed load balancers, billed monthly",
		Filters:     []string{"sort", "name", "label_selector"},
		FilterEnums: map[string][]string{"sort": hcSortNameCreated},
		Create: []hcField{
			hcF("name", true, ""), hcF("load_balancer_type", true, "id or name, e.g. lb11"),
			hcF("location", false, "id or name; or network_zone"), hcF("network_zone", false, "e.g. eu-central"),
			hcF("algorithm", false, "{type: round_robin|least_connections}"),
			hcF("services", false, "[{protocol: tcp|http|https, listen_port, destination_port, proxyprotocol, health_check, http}]"),
			hcF("targets", false, "[{type: server|label_selector|ip, server: {id}, label_selector: {selector}, ip: {ip}, use_private_ip}]"),
			hcF("network", false, "id"), hcF("public_interface", false, ""), hcF("public_net", false, "{ipv4, ipv6} Primary IP ids"), hcF("labels", false, ""),
		},
		CreateClass: hcLifecycle,
		CreateExtra: []string{"action"},
		Update:      hcNameLabels,
		Deletable:   true, DeleteAction: true,
		Actions: []hcAction{
			{Name: "add_service", Class: hcWrite, Body: []hcField{hcF("protocol", true, "tcp, http or https"), hcF("listen_port", true, ""), hcF("destination_port", true, ""), hcF("proxyprotocol", true, ""), hcF("health_check", true, "{protocol, port, interval, timeout, retries, http}"), hcF("http", false, "{cookie_name, cookie_lifetime, certificates, redirect_http, sticky_sessions}")}},
			{Name: "update_service", Class: hcWrite, Body: []hcField{hcF("listen_port", true, "identifies the service"), hcF("protocol", false, ""), hcF("destination_port", false, ""), hcF("proxyprotocol", false, ""), hcF("health_check", false, ""), hcF("http", false, "")}},
			{Name: "delete_service", Class: hcWrite, Body: []hcField{hcF("listen_port", true, "")}},
			{Name: "add_target", Class: hcWrite, Body: []hcField{hcF("type", true, "server, label_selector or ip"), hcF("server", false, "{id}"), hcF("label_selector", false, "{selector}"), hcF("ip", false, "{ip}"), hcF("use_private_ip", false, "")}},
			{Name: "remove_target", Class: hcWrite, Body: []hcField{hcF("type", true, ""), hcF("server", false, "{id}"), hcF("label_selector", false, "{selector}"), hcF("ip", false, "{ip}")}},
			{Name: "attach_to_network", Class: hcWrite, Body: []hcField{hcF("network", true, "id"), hcF("ip", false, ""), hcF("ip_range", false, "")}},
			{Name: "detach_from_network", Class: hcWrite, Body: []hcField{hcF("network", true, "id")}},
			{Name: "change_algorithm", Class: hcWrite, Body: []hcField{hcF("type", true, "round_robin or least_connections")}},
			hcDNSPtr,
			hcProtectionDelete,
			{Name: "change_type", Class: hcLifecycle, Body: []hcField{hcF("load_balancer_type", true, "id or name")}},
			{Name: "enable_public_interface", Class: hcWrite},
			{Name: "disable_public_interface", Class: hcWrite},
		},
		ActionLists: true,
		Metrics:     []string{"open_connections", "connections_per_second", "requests_per_second", "bandwidth"},
		Summary: []string{"id", "name", "type=load_balancer_type.name", "location=location.name", "public_net", "private_net", "algorithm", "services", "targets",
			"protection", "labels", "created"},
	},
	{
		Kind: "load_balancer_type", Collection: "load_balancer_types", Item: "load_balancer_type", IDParam: "id",
		Doc:     "sizes and prices of load balancers",
		Filters: []string{"name"},
		Summary: []string{"id", "name", "description", "max_connections", "max_services", "max_targets", "max_assigned_certificates", "prices", "deprecation"},
	},
	{
		Kind: "certificate", Collection: "certificates", Item: "certificate", IDParam: "id",
		Doc:         "TLS certificates for load balancers, uploaded or managed (Let's Encrypt), free",
		Filters:     []string{"sort", "name", "label_selector", "type"},
		FilterEnums: map[string][]string{"type": {"uploaded", "managed"}, "sort": hcSortNameCreated},
		Create: []hcField{
			hcF("name", true, ""), hcF("type", false, "uploaded (default) or managed"),
			hcF("certificate", false, "PEM, with type uploaded"), hcF("private_key", false, "PEM, with type uploaded"),
			hcF("domain_names", false, "with type managed"), hcF("labels", false, ""),
		},
		CreateClass: hcWrite,
		CreateExtra: []string{"action"},
		Update:      hcNameLabels,
		Deletable:   true,
		Actions:     []hcAction{{Name: "retry", Class: hcWrite, Doc: "retry issuance or renewal of a managed certificate"}},
		ActionLists: true,
		Summary:     []string{"id", "name", "type", "domain_names", "not_valid_before", "not_valid_after", "status", "fingerprint", "used_by", "labels", "created"},
	},
	{
		Kind: "placement_group", Collection: "placement_groups", Item: "placement_group", IDParam: "id",
		Doc:         "spread groups that keep servers on different hosts, free",
		Filters:     []string{"sort", "name", "label_selector", "type"},
		FilterEnums: map[string][]string{"type": {"spread"}, "sort": hcSortNameCreated},
		Create:      []hcField{hcF("name", true, ""), hcF("type", true, "spread"), hcF("labels", false, "")},
		CreateClass: hcWrite,
		CreateExtra: []string{"action"},
		Update:      hcNameLabels,
		Deletable:   true,
		Summary:     []string{"id", "name", "type", "servers", "labels", "created"},
	},
	{
		Kind: "ssh_key", Collection: "ssh_keys", Item: "ssh_key", IDParam: "id",
		Doc:         "public keys to inject into new servers",
		Filters:     []string{"sort", "name", "fingerprint", "label_selector"},
		FilterEnums: map[string][]string{"sort": {"id", "id:asc", "id:desc", "name", "name:asc", "name:desc"}},
		Create:      []hcField{hcF("name", true, ""), hcF("public_key", true, "OpenSSH public key line"), hcF("labels", false, "")},
		CreateClass: hcWrite,
		Update:      hcNameLabels,
		Deletable:   true,
		Summary:     []string{"id", "name", "fingerprint", "public_key", "labels", "created"},
	},
	{
		Kind: "location", Collection: "locations", Item: "location", IDParam: "id",
		Doc:         "data center locations",
		Filters:     []string{"name", "sort"},
		FilterEnums: map[string][]string{"sort": {"id", "id:asc", "id:desc", "name", "name:asc", "name:desc"}},
		Summary:     []string{"id", "name", "description", "country", "city", "network_zone", "latitude", "longitude"},
	},
	{
		Kind: "server_type", Collection: "server_types", Item: "server_type", IDParam: "id",
		Doc:     "sizes with prices per location and availability",
		Filters: []string{"name"},
		Summary: []string{"id", "name", "description", "cores", "memory", "disk", "cpu_type", "architecture", "category", "storage_type", "locations", "prices"},
	},
	{
		Kind: "zone", Collection: "zones", Item: "zone", IDParam: "id_or_name",
		Doc:         "DNS zones (Hetzner DNS, free); records are RRSets, see the rrset tools",
		Filters:     []string{"name", "mode", "label_selector", "sort"},
		FilterEnums: map[string][]string{"mode": {"primary", "secondary"}, "sort": hcSortNameCreated},
		Create: []hcField{
			hcF("name", true, "the domain, lower case, no trailing dot"), hcF("mode", true, "primary or secondary"),
			hcF("ttl", false, "default TTL, 60…2147483647, default 3600"), hcF("labels", false, ""),
			hcF("primary_nameservers", false, "secondary mode: [{address, port, tsig_key, tsig_algorithm}]"),
			hcF("rrsets", false, "primary mode: [{name, type, ttl, records}] created with the zone"),
			hcF("zonefile", false, "primary mode: a BIND zone file imported at creation (not together with rrsets)"),
		},
		CreateClass: hcWrite,
		CreateExtra: []string{"action"},
		Update:      []hcField{hcFieldLabels},
		Deletable:   true, DeleteAction: true,
		Actions: []hcAction{
			{Name: "change_ttl", Class: hcWrite, Body: []hcField{hcF("ttl", true, "")}},
			hcProtectionDelete,
			{Name: "change_primary_nameservers", Class: hcWrite, Body: []hcField{hcF("primary_nameservers", true, "[{address, port, tsig_key, tsig_algorithm}]")}, Doc: "secondary zones"},
			{Name: "import_zonefile", Class: hcLifecycle, Body: []hcField{hcF("zonefile", true, "BIND zone file text")}, Doc: "replaces every RRSet of the zone; fails on change-protected RRSets"},
		},
		ActionLists: true,
		Summary: []string{"id", "name", "mode", "ttl", "status", "record_count", "registrar", "authoritative_nameservers", "primary_nameservers",
			"protection", "labels", "created"},
	},
}

// hcRRSetActions are the actions of a record set, all plain writes: DNS
// records are configuration, the DNS connection's job.
var hcRRSetActions = []hcAction{
	{Name: "set_records", Class: hcWrite, Body: []hcField{hcF("records", true, "[{value, comment}] — replaces all records")}},
	{Name: "add_records", Class: hcWrite, Body: []hcField{hcF("records", true, "[{value, comment}]"), hcF("ttl", false, "")}},
	{Name: "remove_records", Class: hcWrite, Body: []hcField{hcF("records", true, "[{value}] — by value")}},
	{Name: "update_records", Class: hcWrite, Body: []hcField{hcF("records", true, "[{value, comment}] — comments only")}},
	{Name: "change_ttl", Class: hcWrite, Body: []hcField{hcF("ttl", true, "null falls back to the zone's TTL")}},
	{Name: "change_protection", Class: hcWrite, Body: []hcField{hcF("change", true, "Hetzner's own change protection")}},
}

// hcRRSetTypes are the record types the API accepts.
var hcRRSetTypes = []string{"A", "AAAA", "CAA", "CNAME", "DS", "HINFO", "HTTPS", "MX", "NS", "PTR", "RP", "SOA", "SRV", "SVCB", "TLSA", "TXT"}

const (
	hcRRSetListFilters = "name,type,label_selector,sort"
	hcZonePath         = "/zones/{id_or_name}"
	hcRRSetPath        = "/zones/{id_or_name}/rrsets/{rr_name}/{rr_type}"
)

func hcResourceByKind(kind string) *hcResource {
	for i := range hcResources {
		if hcResources[i].Kind == kind {
			return &hcResources[i]
		}
	}
	return nil
}

func hcResourceByCollection(coll string) *hcResource {
	for i := range hcResources {
		if hcResources[i].Collection == coll {
			return &hcResources[i]
		}
	}
	return nil
}

func (r *hcResource) action(name string) *hcAction {
	for i := range r.Actions {
		if r.Actions[i].Name == name {
			return &r.Actions[i]
		}
	}
	return nil
}

func hcRRSetAction(name string) *hcAction {
	for i := range hcRRSetActions {
		if hcRRSetActions[i].Name == name {
			return &hcRRSetActions[i]
		}
	}
	return nil
}

// hcKinds lists the kinds that satisfy pred, in registry order.
func hcKinds(pred func(*hcResource) bool) []string {
	var out []string
	for i := range hcResources {
		if pred == nil || pred(&hcResources[i]) {
			out = append(out, hcResources[i].Kind)
		}
	}
	return out
}

// hcEndpoint is one REST call the type can make: the contract the spec test
// checks. Params are the query parameters the call may send besides page
// and per_page.
type hcEndpoint struct {
	Method string
	Path   string
	Params []string
	Body   []hcField
	Class  hcClass
}

// hcEndpoints builds the complete list of calls from the registry: every
// operation the type can make is here, under a key the tests and the
// pass-through classification share.
func hcEndpoints() map[string]hcEndpoint {
	eps := map[string]hcEndpoint{
		"actions.list":    {Method: http.MethodGet, Path: "/actions", Params: []string{"id"}},
		"actions.get":     {Method: http.MethodGet, Path: "/actions/{id}"},
		"pricing":         {Method: http.MethodGet, Path: "/pricing"},
		"network.members": {Method: http.MethodGet, Path: "/networks/{id}/members", Params: []string{"type", "subnet", "status", "sort"}},
		"zone.zonefile":   {Method: http.MethodGet, Path: hcZonePath + "/zonefile"},
		"rrset.list":      {Method: http.MethodGet, Path: hcZonePath + "/rrsets", Params: strings.Split(hcRRSetListFilters, ",")},
		"rrset.create":    {Method: http.MethodPost, Path: hcZonePath + "/rrsets", Body: []hcField{hcF("name", true, ""), hcF("type", true, ""), hcF("records", true, ""), hcF("ttl", false, ""), hcF("labels", false, "")}, Class: hcWrite},
		"rrset.get":       {Method: http.MethodGet, Path: hcRRSetPath},
		"rrset.update":    {Method: http.MethodPut, Path: hcRRSetPath, Body: []hcField{hcF("labels", false, "")}, Class: hcWrite},
		"rrset.delete":    {Method: http.MethodDelete, Path: hcRRSetPath, Class: hcWrite},
	}
	for _, a := range hcRRSetActions {
		eps["rrset.action."+a.Name] = hcEndpoint{Method: http.MethodPost, Path: hcRRSetPath + "/actions/" + a.Name, Body: a.Body, Class: a.Class}
	}
	for i := range hcResources {
		r := &hcResources[i]
		item := "/" + r.Collection + "/{" + r.IDParam + "}"
		eps[r.Kind+".list"] = hcEndpoint{Method: http.MethodGet, Path: "/" + r.Collection, Params: r.Filters}
		eps[r.Kind+".get"] = hcEndpoint{Method: http.MethodGet, Path: item}
		if r.Create != nil {
			eps[r.Kind+".create"] = hcEndpoint{Method: http.MethodPost, Path: "/" + r.Collection, Body: r.Create, Class: r.CreateClass}
		}
		if r.Update != nil {
			eps[r.Kind+".update"] = hcEndpoint{Method: http.MethodPut, Path: item, Body: r.Update, Class: hcWrite}
		}
		if r.Deletable {
			eps[r.Kind+".delete"] = hcEndpoint{Method: http.MethodDelete, Path: item, Class: hcLifecycle}
		}
		if r.ActionLists {
			eps[r.Kind+".actions"] = hcEndpoint{Method: http.MethodGet, Path: "/" + r.Collection + "/actions", Params: []string{"id", "sort", "status"}}
			eps[r.Kind+".actions.get"] = hcEndpoint{Method: http.MethodGet, Path: "/" + r.Collection + "/actions/{id}"}
			eps[r.Kind+".actions.of"] = hcEndpoint{Method: http.MethodGet, Path: item + "/actions", Params: []string{"sort", "status"}}
		}
		for _, a := range r.Actions {
			eps[r.Kind+".action."+a.Name] = hcEndpoint{Method: http.MethodPost, Path: item + "/actions/" + a.Name, Body: a.Body, Class: a.Class}
		}
		if r.Metrics != nil {
			eps[r.Kind+".metrics"] = hcEndpoint{Method: http.MethodGet, Path: item + "/metrics", Params: []string{"type", "start", "end", "step"}}
		}
	}
	return eps
}

// --- Client ---

// HetznerCloud wraps a Hetzner Cloud project as MCP tools.
type HetznerCloud struct {
	name           string
	baseURL        string // https://api.hetzner.cloud, no path
	token          string
	readOnly       bool
	allowLifecycle bool
	guards         []config.ResourceGuard
	client         *http.Client
	poll           []time.Duration // swappable for tests

	// mu guards the rate-limit figures of the most recent response.
	mu            sync.Mutex
	rateRemaining int
	rateLimit     int
	rateReset     time.Time
	rateSeen      bool
}

// NewHetznerCloud creates a hetzner-cloud connection from config.
func NewHetznerCloud(conn config.Connection, dialer Dialer) (*HetznerCloud, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("hetzner-cloud: token (API token of the project) is required")
	}
	if err := config.ValidateHetznerCloud(conn); err != nil {
		return nil, fmt.Errorf("hetzner-cloud: %w", err)
	}
	baseURL, err := config.HetznerCloudBaseURL(conn.URL)
	if err != nil {
		return nil, fmt.Errorf("hetzner-cloud: %w", err)
	}
	guards, err := config.ParseResourceGuards(conn.ProtectedResources)
	if err != nil {
		return nil, fmt.Errorf("hetzner-cloud: protected_resources: %w", err)
	}
	transport := &http.Transport{}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return &HetznerCloud{
		name:           conn.Name,
		baseURL:        baseURL,
		token:          conn.Token,
		readOnly:       conn.ReadOnly,
		allowLifecycle: conn.AllowLifecycle && !conn.ReadOnly,
		guards:         guards,
		// Redirects are not followed: the token would travel to wherever the
		// response points, and the API answers no 3xx.
		client: &http.Client{Transport: transport, Timeout: hcRequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		poll: hcPollIntervals,
	}, nil
}

// ReadOnly reports whether the connection registers reading tools only.
func (h *HetznerCloud) ReadOnly() bool { return h.readOnly }

// hcAPIError is the API's error envelope with the HTTP status.
type hcAPIError struct {
	Status  int
	Code    string
	Message string
	Details json.RawMessage
	Reset   time.Time // when the request budget recovers, on rate_limit_exceeded
}

func (e *hcAPIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hetzner API %d", e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if len(e.Details) > 0 && string(e.Details) != "null" && string(e.Details) != "{}" {
		d := string(e.Details)
		if len(d) > 600 {
			d = d[:600] + "…"
		}
		b.WriteString(" — details " + d)
	}
	switch e.Code {
	case "rate_limit_exceeded":
		if !e.Reset.IsZero() {
			fmt.Fprintf(&b, " (the budget refills by one request per second and is full again at %s)", e.Reset.UTC().Format(time.RFC3339))
		}
	case "protected":
		b.WriteString(" (Hetzner's own protection is set on this resource — see its protection field and the change_protection action)")
	case "locked":
		b.WriteString(" (another action is running on this resource — wait for it, see list_actions)")
	case "deprecated_api_endpoint":
		b.WriteString(" (Hetzner removed this API functionality — the registry of this mux build is behind; see the changelog link in details)")
	case "unauthorized":
		b.WriteString(" (the token is invalid or revoked — tokens are per project, Hetzner Console → Security → API Tokens)")
	case "forbidden":
		b.WriteString(" (the token lacks the permission — a Read token cannot write)")
	}
	return b.String()
}

// hcResponse is one answer of the API.
type hcResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// do sends one request under /v1 with the connection's token and reads the
// body. A conflict (409: the resource changed during the request, the API
// asks for a retry and applied nothing) is retried once for every method; a
// gateway failure (502, 504) and a transport failure are retried once for
// GET and PUT only — a POST creates a resource or starts an action, a DELETE
// answers 404 the second time, and the first request may have gone through.
// A 429 is never retried: the budget refills by one request per second, the
// error says when it is full again.
func (h *HetznerCloud) do(ctx context.Context, method, path string, query url.Values, body any) (*hcResponse, error) {
	var payload []byte
	if body != nil {
		var err error
		if raw, ok := body.(json.RawMessage); ok {
			payload = raw
		} else if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
	}
	u := h.baseURL + hcVersionPath + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rdr)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "mux hetzner-cloud connection")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := h.client.Do(req)
		if err != nil {
			if attempt == 0 && ctx.Err() == nil && hcRetryNetError(method, err) {
				if hcSleep(ctx, time.Second) {
					continue
				}
			}
			return nil, fmt.Errorf("request failed: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, hcMaxBody+1))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if len(data) > hcMaxBody {
			return nil, fmt.Errorf("response larger than %d MiB", hcMaxBody>>20)
		}
		h.noteRateLimit(resp.Header)
		if attempt == 0 && ctx.Err() == nil {
			if wait, ok := hcRetryAfter(method, resp.StatusCode, data); ok && hcSleep(ctx, wait) {
				continue
			}
		}
		return &hcResponse{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
	}
}

// hcIdempotent reports whether a method may be sent twice and read the same
// way: a GET reads, a PUT sets the same fields again. A POST is not — it
// creates a resource or starts an action, and a lost response does not mean
// a lost request. A DELETE is not retried either: the second attempt would
// answer 404 for a delete that went through, and the action of the first
// answer (servers, load balancers, zones) would be lost — the caller sees the
// gateway error and reads the resource.
func hcIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPut:
		return true
	}
	return false
}

// hcRetryNetError decides whether a transport failure is retried once: only
// for idempotent methods, and not after a timeout (60 s are not worth a
// second minute). Every error http.Client returns is a *url.Error and thus a
// net.Error, so the method, not the error type, is the deciding factor.
func hcRetryNetError(method string, err error) bool {
	if !hcIdempotent(method) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return true
}

// hcRetryAfter decides whether a response is retried and after how long:
// conflict (409, the API applied nothing and asks for a retry) after half a
// second for every method; bad gateway and gateway timeout after a second
// for idempotent methods. Everything else — the rate limit included — is
// reported.
func hcRetryAfter(method string, status int, data []byte) (time.Duration, bool) {
	switch status {
	case http.StatusConflict:
		if e := hcParseError(status, data); e.Code == "conflict" {
			return 500 * time.Millisecond, true
		}
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		if hcIdempotent(method) {
			return time.Second, true
		}
	}
	return 0, false
}

func hcSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (h *HetznerCloud) noteRateLimit(hdr http.Header) {
	rem, err1 := strconv.Atoi(hdr.Get("RateLimit-Remaining"))
	lim, err2 := strconv.Atoi(hdr.Get("RateLimit-Limit"))
	if err1 != nil || err2 != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rateRemaining, h.rateLimit, h.rateSeen = rem, lim, true
	if ts, err := strconv.ParseInt(hdr.Get("RateLimit-Reset"), 10, 64); err == nil {
		h.rateReset = time.Unix(ts, 0)
	}
}

// rateNote is the line a result carries when the request budget runs low.
func (h *HetznerCloud) rateNote() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.rateSeen || h.rateRemaining >= hcRateLimitWarn {
		return ""
	}
	note := fmt.Sprintf("rate limit: %d of %d requests left this hour", h.rateRemaining, h.rateLimit)
	if !h.rateReset.IsZero() {
		note += ", full again at " + h.rateReset.UTC().Format(time.RFC3339)
	}
	return note
}

// hcParseError reads the API's error envelope; a body that is not one
// becomes a message from the status and the first bytes.
func hcParseError(status int, data []byte) *hcAPIError {
	var env struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	e := &hcAPIError{Status: status}
	if json.Unmarshal(data, &env) == nil && (env.Error.Code != "" || env.Error.Message != "") {
		e.Code, e.Message, e.Details = env.Error.Code, env.Error.Message, env.Error.Details
		return e
	}
	snippet := strings.TrimSpace(string(data))
	if len(snippet) > 300 {
		snippet = snippet[:300] + "…"
	}
	e.Message = http.StatusText(status)
	if snippet != "" {
		e.Message += ": " + snippet
	}
	return e
}

func (h *HetznerCloud) errorOf(resp *hcResponse) *hcAPIError {
	e := hcParseError(resp.Status, resp.Body)
	if e.Code == "rate_limit_exceeded" {
		if ts, err := strconv.ParseInt(resp.Header.Get("RateLimit-Reset"), 10, 64); err == nil {
			e.Reset = time.Unix(ts, 0)
		}
	}
	return e
}

// callJSON performs a call, requires a 2xx status and decodes the body into
// out (nil out discards it; a 204 leaves out untouched).
func (h *HetznerCloud) callJSON(ctx context.Context, method, path string, query url.Values, body, out any) (*hcResponse, error) {
	resp, err := h.do(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return resp, h.errorOf(resp)
	}
	if out == nil || len(resp.Body) == 0 {
		return resp, nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return resp, fmt.Errorf("decode Hetzner response: %w", err)
	}
	return resp, nil
}

// --- Paging ---

// hcPagination is meta.pagination of a list response.
type hcPagination struct {
	Page         int  `json:"page"`
	PerPage      int  `json:"per_page"`
	PreviousPage *int `json:"previous_page"`
	NextPage     *int `json:"next_page"`
	LastPage     *int `json:"last_page"`
	TotalEntries *int `json:"total_entries"`
}

// listPage GETs one page of a collection: the items under wrapper and the
// pagination.
func (h *HetznerCloud) listPage(ctx context.Context, path, wrapper string, query url.Values) ([]map[string]any, *hcPagination, error) {
	var raw map[string]json.RawMessage
	if _, err := h.callJSON(ctx, http.MethodGet, path, query, nil, &raw); err != nil {
		return nil, nil, err
	}
	var items []map[string]any
	if data, ok := raw[wrapper]; ok {
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, nil, fmt.Errorf("decode %s: %w", wrapper, err)
		}
	} else {
		return nil, nil, fmt.Errorf("response carries no %q", wrapper)
	}
	var meta struct {
		Pagination *hcPagination `json:"pagination"`
	}
	if data, ok := raw["meta"]; ok {
		_ = json.Unmarshal(data, &meta)
	}
	return items, meta.Pagination, nil
}

// listAll follows next_page from the first page, at most hcMaxPages pages of
// hcPerPageMax items. complete is false when the cap stopped the walk.
func (h *HetznerCloud) listAll(ctx context.Context, path, wrapper string, query url.Values) (items []map[string]any, total *int, complete bool, err error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", strconv.Itoa(hcPerPageMax))
	page := 1
	for n := 0; n < hcMaxPages; n++ {
		q.Set("page", strconv.Itoa(page))
		got, pg, err := h.listPage(ctx, path, wrapper, q)
		if err != nil {
			return items, total, false, err
		}
		items = append(items, got...)
		if pg != nil && pg.TotalEntries != nil {
			total = pg.TotalEntries
		}
		if pg == nil || pg.NextPage == nil || *pg.NextPage <= page || len(got) == 0 {
			return items, total, true, nil
		}
		page = *pg.NextPage
	}
	return items, total, false, nil
}

// --- Names and ids ---

func hcNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// resolve turns an id or a name into the id the paths take. A numeric
// argument is the id. A name is looked up with the collection's name filter
// (names are unique per project); zones take the name in the path as is.
// When a lookup fetched the object it is returned too, so a guard check
// needs no second read.
func (h *HetznerCloud) resolve(ctx context.Context, res *hcResource, idOrName string) (string, map[string]any, error) {
	v := strings.TrimSpace(idOrName)
	if v == "" {
		return "", nil, fmt.Errorf("%s: id or name is required", res.Kind)
	}
	if hcNumeric(v) {
		return v, nil, nil
	}
	if res.IDParam == "id_or_name" {
		if err := hcPathValue(v, res.Kind); err != nil {
			return "", nil, err
		}
		return v, nil, nil
	}
	q := url.Values{"name": {v}, "per_page": {"2"}}
	items, _, err := h.listPage(ctx, "/"+res.Collection, res.Collection, q)
	if err != nil {
		return "", nil, err
	}
	switch len(items) {
	case 0:
		return "", nil, fmt.Errorf("no %s named %q in this project", res.Kind, v)
	case 1:
		return hcID(items[0]), items[0], nil
	default:
		return "", nil, fmt.Errorf("%d %ss match the name %q — pass the id", len(items), res.Kind, v)
	}
}

// hcID renders the id of an object as the paths take it.
func hcID(obj map[string]any) string {
	switch v := obj["id"].(type) {
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		return v.String()
	case string:
		return v
	}
	return ""
}

func hcName(obj map[string]any) string {
	s, _ := obj["name"].(string)
	return s
}

// --- Guards ---

// permit checks the class of an operation against read_only and
// allow_lifecycle. what names the operation for the message.
func (h *HetznerCloud) permit(class hcClass, what string) error {
	switch class {
	case hcRead:
		return nil
	case hcWrite:
		if h.readOnly {
			return fmt.Errorf("%s is refused: this connection is read-only (read_only = true)", what)
		}
		return nil
	default:
		if h.readOnly {
			return fmt.Errorf("%s is refused: this connection is read-only (read_only = true)", what)
		}
		if !h.allowLifecycle {
			return fmt.Errorf("%s is refused: it costs money or destroys data or availability, and this connection does not allow lifecycle operations (allow_lifecycle is off) — a human switches it on in mux's configuration for this connection", what)
		}
		return nil
	}
}

// protect refuses an operation on a protected resource. The name comes from
// the object when the caller has it, from the path when the id is a name
// (zones), otherwise from one read of the resource.
func (h *HetznerCloud) protect(ctx context.Context, res *hcResource, id string, obj map[string]any) error {
	if len(h.guards) == 0 {
		return nil
	}
	name := ""
	if obj != nil {
		name = hcName(obj)
	} else {
		// One read — also for a zone addressed by name: the guard matches the
		// name Hetzner knows, not the spelling the caller used (trailing dot,
		// upper case, punycode).
		var raw map[string]map[string]any
		if _, err := h.callJSON(ctx, http.MethodGet, "/"+res.Collection+"/"+url.PathEscape(id), nil, nil, &raw); err != nil {
			var apiErr *hcAPIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				return err // the resource does not exist — that is the answer, not a guard failure
			}
			return fmt.Errorf("cannot check the protected_resources guard — reading %s %s failed: %w", res.Kind, id, err)
		}
		obj = raw[res.Item]
		if obj == nil {
			// Fail closed: an answer without the item is not the resource the
			// guard has to judge.
			return fmt.Errorf("cannot check the protected_resources guard — reading %s %s answered without a %s", res.Kind, id, res.Item)
		}
		name = hcName(obj)
	}
	// Snapshots and backups carry no name — their identity is the
	// description; an image guard matches either.
	candidates := []string{name}
	if res.Kind == "image" {
		if d, _ := obj["description"].(string); d != "" {
			candidates = append(candidates, d)
		}
	}
	for _, g := range h.guards {
		for _, c := range candidates {
			if g.Matches(res.Kind, c) {
				return fmt.Errorf("%s %q is protected on this connection (protected_resources) — mux does not change, act on or delete it", res.Kind, c)
			}
		}
	}
	return nil
}

// --- Actions ---

// hcSplitActions takes the actions out of a response — action, actions,
// next_actions — and keeps everything else as extra (root_password, image,
// wss_url, password, the created item).
func hcSplitActions(data []byte) (actions []map[string]any, extra map[string]any, err error) {
	var raw map[string]json.RawMessage
	if len(data) == 0 {
		return nil, map[string]any{}, nil
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("decode Hetzner response: %w", err)
	}
	extra = map[string]any{}
	// Fixed order: the action of the call first, then the lists.
	if v, ok := raw["action"]; ok {
		var a map[string]any
		if json.Unmarshal(v, &a) == nil && a != nil {
			actions = append(actions, a)
		}
	}
	for _, k := range []string{"actions", "next_actions"} {
		if v, ok := raw[k]; ok {
			var list []map[string]any
			if json.Unmarshal(v, &list) == nil {
				actions = append(actions, list...)
			}
		}
	}
	for k, v := range raw {
		switch k {
		case "action", "actions", "next_actions":
		default:
			var x any
			_ = json.Unmarshal(v, &x)
			extra[k] = x
		}
	}
	return actions, extra, nil
}

// hcActionSummary reduces an action to what matters.
func hcActionSummary(a map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "command", "status", "progress", "started", "finished", "resources", "error"} {
		if v, ok := a[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

func hcActionStatus(a map[string]any) string {
	s, _ := a["status"].(string)
	return s
}

// waitActions polls GET /actions?id=… until no action is running or the
// timeout passes, pacing itself with h.poll. The returned list carries the
// latest state of every action; done is false when the timeout struck.
func (h *HetznerCloud) waitActions(ctx context.Context, actions []map[string]any, timeout time.Duration) ([]map[string]any, bool, error) {
	byID := map[string]map[string]any{}
	var order []string
	running := map[string]bool{}
	for _, a := range actions {
		id := hcID(a)
		if id == "" {
			continue
		}
		byID[id] = a
		order = append(order, id)
		if hcActionStatus(a) == "running" {
			running[id] = true
		}
	}
	deadline := time.Now().Add(timeout)
	for attempt := 0; len(running) > 0; attempt++ {
		wait := h.poll[min(attempt, len(h.poll)-1)]
		if remaining := time.Until(deadline); remaining <= 0 {
			break
		} else if wait > remaining {
			wait = remaining
		}
		if !hcSleep(ctx, wait) {
			return hcOrdered(byID, order), false, ctx.Err()
		}
		ids := make([]string, 0, len(running))
		for id := range running {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for start := 0; start < len(ids); start += hcActionIDsChunk {
			chunk := ids[start:min(start+hcActionIDsChunk, len(ids))]
			// GET /actions takes the ids and nothing else — no paging, no sort.
			items, _, err := h.listPage(ctx, "/actions", "actions", url.Values{"id": chunk})
			if err != nil {
				return hcOrdered(byID, order), false, err
			}
			for _, a := range items {
				id := hcID(a)
				byID[id] = a
				if hcActionStatus(a) != "running" {
					delete(running, id)
				}
			}
		}
	}
	return hcOrdered(byID, order), len(running) == 0, nil
}

func hcOrdered(byID map[string]map[string]any, order []string) []map[string]any {
	out := make([]map[string]any, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// --- Summaries ---

// hcSummarize picks the fields of a compact listing: each entry is a path
// into the object ("server_type.name"), optionally with an alias
// ("ipv4=public_net.ipv4.ip"). Missing fields are left out.
func hcSummarize(obj map[string]any, fields []string) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		alias, path, found := strings.Cut(f, "=")
		if !found {
			path, alias = alias, f
		}
		if v, ok := hcLookup(obj, path); ok && v != nil {
			out[alias] = v
		}
	}
	return out
}

func hcLookup(obj map[string]any, path string) (any, bool) {
	var cur any = obj
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// --- Pass-through classification ---

// hcTarget is what a pass-through path addresses, as the guards see it.
type hcTarget struct {
	Class hcClass
	Known bool        // the method and path are in the registry
	Res   *hcResource // the resource acted on, when the path names one
	ID    string      // its id (or name) in the path, "" for collection calls
	What  string      // for messages: "DELETE /servers/42"
}

// hcAPIPath validates a pass-through path: plain, under /v1/, no dot
// segments, no encoding tricks. The query string is kept for GET.
func hcAPIPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	pathPart, query, _ := strings.Cut(p, "?")
	if strings.ContainsAny(pathPart, "%#\\{}") {
		return "", fmt.Errorf("path %q: write it plain — no percent-encoding, no #, no braces, no backslash", raw)
	}
	if !strings.HasPrefix(pathPart, hcVersionPath+"/") {
		return "", fmt.Errorf("path must start with %s/, got %q", hcVersionPath, raw)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(pathPart, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("path %q has an empty or dot segment", raw)
		}
	}
	if query != "" {
		return pathPart + "?" + query, nil
	}
	return pathPart, nil
}

// hcHasLifecycleKey reports whether an update body carries one of the
// resource's UpdateLifecycle keys — compared without regard to case, as a
// lenient JSON decoder on the server would.
func hcHasLifecycleKey(res *hcResource, body map[string]any) bool {
	for _, k := range res.UpdateLifecycle {
		for bk := range body {
			if strings.EqualFold(bk, k) {
				return true
			}
		}
	}
	return false
}

// hcPathValue validates a value that goes into a path segment (a zone, a
// record set name): no slash, no whitespace or control character, no dot
// segment — a value that would change the path's shape is refused before
// escaping, whatever the server would make of it.
func hcPathValue(v, what string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("%s is required", what)
	}
	if v == "." || v == ".." {
		return fmt.Errorf("%s %q is a dot segment", what, v)
	}
	for _, r := range v {
		if r == '/' || r == '\\' || r == '%' || r == '?' || r == '#' || r <= ' ' || r == 0x7f {
			return fmt.Errorf("%s %q carries a character that cannot stand in a path (slash, percent, whitespace, control)", what, v)
		}
	}
	return nil
}

// hcClassify maps method and /v1 path onto the registry. A GET is always a
// read. A write the registry does not know is reported as unknown — the
// caller admits it only with allow_lifecycle, because mux cannot tell what
// it does. The body matters for one case: a PUT whose body carries a key
// the resource lists under UpdateLifecycle (an image's type) is a lifecycle
// operation, as the update tool treats it.
func hcClassify(method, path string, body map[string]any) hcTarget {
	path, _, _ = strings.Cut(path, "?")
	segs := strings.Split(strings.Trim(strings.TrimPrefix(path, hcVersionPath), "/"), "/")
	t := hcTarget{What: method + " " + path}
	if method == http.MethodGet {
		t.Class, t.Known = hcRead, true
		return t
	}
	if len(segs) == 0 || segs[0] == "" {
		return t
	}
	if len(segs) >= 2 && segs[1] != "actions" {
		t.ID = segs[1] // whatever the shape, the second segment addresses a resource of the collection
	}
	// DNS record sets: /zones/{zone}/rrsets[/{name}/{type}[/actions/{a}]]
	if segs[0] == "zones" && len(segs) >= 3 && segs[2] == "rrsets" {
		t.Res, t.ID = hcResourceByKind("zone"), segs[1]
		switch {
		case len(segs) == 3 && method == http.MethodPost,
			len(segs) == 5 && (method == http.MethodPut || method == http.MethodDelete):
			t.Class, t.Known = hcWrite, true
		case len(segs) == 7 && segs[5] == "actions" && method == http.MethodPost:
			if a := hcRRSetAction(segs[6]); a != nil {
				t.Class, t.Known = a.Class, true
			}
		}
		return t
	}
	res := hcResourceByCollection(segs[0])
	if res == nil {
		return t
	}
	t.Res = res
	switch {
	case len(segs) == 1 && method == http.MethodPost && res.Create != nil:
		t.Class, t.Known = res.CreateClass, true
	case len(segs) == 2 && segs[1] != "actions":
		switch method {
		case http.MethodPut:
			if res.Update != nil {
				t.Class, t.Known = hcWrite, true
				if hcHasLifecycleKey(res, body) {
					t.Class = hcLifecycle
				}
			}
		case http.MethodDelete:
			if res.Deletable {
				t.Class, t.Known = hcLifecycle, true
			}
		}
	case len(segs) == 4 && segs[2] == "actions" && method == http.MethodPost:
		if a := res.action(segs[3]); a != nil {
			t.Class, t.Known = a.Class, true
		}
	}
	return t
}
