package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// calendarTools returns the Outlook Calendar MCP tools.
func (mg *MicrosoftGraph) calendarTools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("cal_calendar_view",
				mcp.WithDescription("List events in a time range. Recurring series are expanded into individual occurrences. Times in response use UTC unless the event itself specifies a time zone."),
				mcp.WithString("start", mcp.Required(), mcp.Description("Range start in ISO 8601 (e.g. 2026-04-30T00:00:00 or 2026-04-30T00:00:00Z)")),
				mcp.WithString("end", mcp.Required(), mcp.Description("Range end in ISO 8601, exclusive")),
				mcp.WithNumber("top", mcp.Description("Max events (default 100, max 250)")),
			),
			Handler: mg.handleCalCalendarView,
		},
		{
			Tool: mcp.NewTool("cal_get_event",
				mcp.WithDescription("Get a single event by ID, including body, attendees, and online meeting info."),
				mcp.WithString("event_id", mcp.Required(), mcp.Description("Event ID")),
			),
			Handler: mg.handleCalGetEvent,
		},
		{
			Tool: mcp.NewTool("cal_create_event",
				mcp.WithDescription("Create a calendar event. Sends invitations to attendees if any are listed."),
				mcp.WithString("subject", mcp.Required(), mcp.Description("Event subject")),
				mcp.WithString("start", mcp.Required(), mcp.Description("Start datetime in ISO 8601 (without offset, e.g. 2026-05-01T14:00:00)")),
				mcp.WithString("end", mcp.Required(), mcp.Description("End datetime in ISO 8601 (without offset)")),
				mcp.WithString("time_zone", mcp.Description("IANA or Windows time zone for start/end (default: UTC). Example: Europe/Berlin")),
				mcp.WithString("body", mcp.Description("Event body (plain text or HTML)")),
				mcp.WithString("location", mcp.Description("Location display name")),
				mcp.WithString("attendees", mcp.Description("Comma-separated attendee email addresses")),
				mcp.WithBoolean("online_meeting", mcp.Description("Create as Teams online meeting (default: false)")),
				mcp.WithBoolean("all_day", mcp.Description("All-day event (default: false). If true, start/end must be midnight in the same time zone.")),
			),
			Handler: mg.handleCalCreateEvent,
		},
		{
			Tool: mcp.NewTool("cal_update_event",
				mcp.WithDescription("Update fields on an existing event. Only provided fields are changed."),
				mcp.WithString("event_id", mcp.Required(), mcp.Description("Event ID")),
				mcp.WithString("subject", mcp.Description("New subject")),
				mcp.WithString("start", mcp.Description("New start datetime in ISO 8601 (without offset). Pass time_zone explicitly when changing only one of start/end to avoid implicit UTC.")),
				mcp.WithString("end", mcp.Description("New end datetime in ISO 8601 (without offset). Pass time_zone explicitly when changing only one of start/end to avoid implicit UTC.")),
				mcp.WithString("time_zone", mcp.Description("Time zone applied to start/end if either is provided (default: UTC). Ignored when neither start nor end is set.")),
				mcp.WithString("body", mcp.Description("New body")),
				mcp.WithString("location", mcp.Description("New location display name")),
				mcp.WithString("attendees", mcp.Description("Comma-separated attendee emails — replaces the existing list")),
			),
			Handler: mg.handleCalUpdateEvent,
		},
		{
			Tool: mcp.NewTool("cal_delete_event",
				mcp.WithDescription("Delete an event. For meetings the organizer set up, this cancels and notifies attendees."),
				mcp.WithString("event_id", mcp.Required(), mcp.Description("Event ID")),
			),
			Handler: mg.handleCalDeleteEvent,
		},
		{
			Tool: mcp.NewTool("cal_get_schedule",
				mcp.WithDescription("Get free/busy availability for one or more mailboxes in a time range. Useful for finding meeting slots."),
				mcp.WithString("schedules", mcp.Required(), mcp.Description("Comma-separated email addresses to query (max 20)")),
				mcp.WithString("start", mcp.Required(), mcp.Description("Range start in ISO 8601 (without offset)")),
				mcp.WithString("end", mcp.Required(), mcp.Description("Range end in ISO 8601 (without offset)")),
				mcp.WithString("time_zone", mcp.Description("Time zone for start/end (default: UTC)")),
				mcp.WithNumber("interval", mcp.Description("Slot length in minutes (default: 30, min: 5)")),
			),
			Handler: mg.handleCalGetSchedule,
		},
	}
}

// --- Calendar handlers ---

func (mg *MicrosoftGraph) handleCalCalendarView(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, _ := req.RequireString("start")
	end, _ := req.RequireString("end")
	if start == "" || end == "" {
		return mcp.NewToolResultError("start and end are required"), nil
	}

	top := 100
	if v, ok := req.GetArguments()["top"].(float64); ok && v > 0 {
		top = int(v)
		if top > 250 {
			top = 250
		}
	}

	fields := "id,subject,start,end,location,organizer,attendees,isAllDay,isCancelled,isOnlineMeeting,onlineMeeting,seriesMasterId,type,bodyPreview,webLink"
	path := fmt.Sprintf("/me/calendarView?startDateTime=%s&endDateTime=%s&$select=%s&$orderby=start/dateTime&$top=%d",
		url.QueryEscape(start), url.QueryEscape(end), fields, top)
	return mg.graphGETValuesPaged(ctx, path)
}

func (mg *MicrosoftGraph) handleCalGetEvent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	eventID, _ := req.RequireString("event_id")
	if eventID == "" {
		return mcp.NewToolResultError("event_id is required"), nil
	}
	return mg.graphGET(ctx, "/me/events/"+url.PathEscape(eventID))
}

func (mg *MicrosoftGraph) handleCalCreateEvent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	subject, _ := req.RequireString("subject")
	start, _ := req.RequireString("start")
	end, _ := req.RequireString("end")
	if subject == "" || start == "" || end == "" {
		return mcp.NewToolResultError("subject, start, and end are required"), nil
	}

	tz := req.GetString("time_zone", "UTC")
	payload := map[string]any{
		"subject": subject,
		"start":   graphDateTime(start, tz),
		"end":     graphDateTime(end, tz),
	}

	if body := req.GetString("body", ""); body != "" {
		payload["body"] = map[string]string{
			"contentType": "HTML",
			"content":     plainToHTML(body),
		}
	}

	if loc := req.GetString("location", ""); loc != "" {
		payload["location"] = map[string]string{"displayName": loc}
	}

	if att := req.GetString("attendees", ""); att != "" {
		attendees := parseAttendees(att)
		if len(attendees) == 0 {
			return mcp.NewToolResultError("attendees must contain at least one valid email address"), nil
		}
		payload["attendees"] = attendees
	}

	if v, ok := req.GetArguments()["online_meeting"].(bool); ok && v {
		payload["isOnlineMeeting"] = true
		payload["onlineMeetingProvider"] = "teamsForBusiness"
	}

	if v, ok := req.GetArguments()["all_day"].(bool); ok && v {
		payload["isAllDay"] = true
	}

	data, status, err := mg.doGraph(ctx, http.MethodPost, "/me/events", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusCreated {
		return mcp.NewToolResultError(fmt.Sprintf("create event failed (HTTP %d): %s", status, string(data))), nil
	}

	var event struct {
		ID      string `json:"id"`
		WebLink string `json:"webLink"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return mcp.NewToolResultError("parse event: " + err.Error()), nil
	}

	return jsonResult(map[string]any{"event_id": event.ID, "web_link": event.WebLink, "subject": subject})
}

func (mg *MicrosoftGraph) handleCalUpdateEvent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	eventID, _ := req.RequireString("event_id")
	if eventID == "" {
		return mcp.NewToolResultError("event_id is required"), nil
	}

	payload := map[string]any{}

	if v := req.GetString("subject", ""); v != "" {
		payload["subject"] = v
	}

	start := req.GetString("start", "")
	end := req.GetString("end", "")
	if start != "" || end != "" {
		tz := req.GetString("time_zone", "UTC")
		if start != "" {
			payload["start"] = graphDateTime(start, tz)
		}
		if end != "" {
			payload["end"] = graphDateTime(end, tz)
		}
	}

	if v := req.GetString("body", ""); v != "" {
		payload["body"] = map[string]string{
			"contentType": "HTML",
			"content":     plainToHTML(v),
		}
	}

	if v := req.GetString("location", ""); v != "" {
		payload["location"] = map[string]string{"displayName": v}
	}

	if v := req.GetString("attendees", ""); v != "" {
		attendees := parseAttendees(v)
		if len(attendees) == 0 {
			return mcp.NewToolResultError("attendees must contain at least one valid email address"), nil
		}
		payload["attendees"] = attendees
	}

	if len(payload) == 0 {
		return mcp.NewToolResultError("no fields to update — provide at least one of: subject, start, end, body, location, attendees"), nil
	}

	data, status, err := mg.doGraph(ctx, http.MethodPatch, "/me/events/"+url.PathEscape(eventID), payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("update event failed (HTTP %d): %s", status, string(data))), nil
	}

	return jsonResult(map[string]any{"event_id": eventID, "updated": true})
}

func (mg *MicrosoftGraph) handleCalDeleteEvent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	eventID, _ := req.RequireString("event_id")
	if eventID == "" {
		return mcp.NewToolResultError("event_id is required"), nil
	}

	data, status, err := mg.doGraph(ctx, http.MethodDelete, "/me/events/"+url.PathEscape(eventID), nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("delete event failed (HTTP %d): %s", status, string(data))), nil
	}

	return jsonResult(map[string]any{"event_id": eventID, "deleted": true})
}

func (mg *MicrosoftGraph) handleCalGetSchedule(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	schedules, _ := req.RequireString("schedules")
	start, _ := req.RequireString("start")
	end, _ := req.RequireString("end")
	if schedules == "" || start == "" || end == "" {
		return mcp.NewToolResultError("schedules, start, and end are required"), nil
	}

	addrs := splitAndTrim(schedules)
	if len(addrs) == 0 {
		return mcp.NewToolResultError("schedules must contain at least one email address"), nil
	}
	if len(addrs) > 20 {
		return mcp.NewToolResultError("schedules: max 20 addresses per call"), nil
	}

	interval := 30
	if v, ok := req.GetArguments()["interval"].(float64); ok && v >= 5 {
		interval = int(v)
		if interval > 1440 {
			interval = 1440
		}
	}

	tz := req.GetString("time_zone", "UTC")
	payload := map[string]any{
		"schedules":                addrs,
		"startTime":                graphDateTime(start, tz),
		"endTime":                  graphDateTime(end, tz),
		"availabilityViewInterval": interval,
	}

	data, status, err := mg.doGraph(ctx, http.MethodPost, "/me/calendar/getSchedule", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("getSchedule failed (HTTP %d): %s", status, string(data))), nil
	}

	var resp struct {
		Value []any `json:"value"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("parse: " + err.Error()), nil
	}
	return jsonResult(map[string]any{
		"interval_minutes": interval,
		"schedules":        resp.Value,
	})
}

// --- Calendar helpers ---

// graphDateTime builds the Graph dateTimeTimeZone object used by event start/end
// and getSchedule startTime/endTime payloads.
func graphDateTime(value, tz string) map[string]string {
	return map[string]string{"dateTime": value, "timeZone": tz}
}

// parseAttendees turns a comma-separated address list into Graph attendee objects.
// Each attendee is treated as required; this matches Outlook's default UI behavior.
func parseAttendees(s string) []map[string]any {
	addrs := splitAndTrim(s)
	if len(addrs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, map[string]any{
			"emailAddress": map[string]string{"address": a},
			"type":         "required",
		})
	}
	return out
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}
