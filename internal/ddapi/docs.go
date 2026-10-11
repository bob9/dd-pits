package ddapi

// swaggerUIPage is the Swagger UI shell served at /docs. It loads the UI assets
// from a CDN (the docs page is a developer aid, viewed when integrating) and
// points them at the locally-served openapi.json, so "Try it out" executes
// against this same server. The API endpoints themselves need no internet.
const swaggerUIPage = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>DroneDash API — Swagger UI</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.onload = function () {
      window.ui = SwaggerUIBundle({
        url: 'openapi.json',
        dom_id: '#swagger-ui',
        deepLinking: true,
        defaultModelsExpandDepth: 1,
        tryItOutEnabled: true,
        displayRequestDuration: true,
      });
    };
  </script>
</body>
</html>`

// openapiSpec describes the shared DroneDash read-API. The same spec is served by
// the offline dd-pits agent and the cloud server — the only difference is the
// optional ?meeting= selector (cloud: pick a meeting; local: ignored, there is
// only one). Omitting it always resolves the active/current meeting, so the
// calls are identical across both.
const openapiSpec = `{
  "openapi": "3.0.3",
  "info": {
    "title": "DroneDash Read API",
    "version": "1.2.0",
    "description": "Live race state served identically by the offline dd-pits venue agent and the cloud platform server.\n\nConventions:\n- JSON keys are PascalCase (Go field names; no json tags).\n- Permissive CORS (Access-Control-Allow-Origin: *) on every endpoint — browser apps on any origin (OBS overlays, custom pit displays) can fetch directly.\n- ?pretty=1 indents any JSON endpoint.\n- Times are seconds (floats); StartMs is unix epoch milliseconds.\n\nMeeting selection:\n- Every data endpoint takes an OPTIONAL ?meeting=<id> (alias ?event=<id>). Omit it and the API resolves the CURRENT/active meeting — so the same call works on the local agent (one meeting) and the cloud (many). Use /current-meeting.json to discover the active meeting id.\n- Before a meeting loads, data endpoints return {\"waiting\":true} (and /meeting.json + /current-meeting.json return {\"loaded\":false})."
  },
  "servers": [
    { "url": "__BASE__", "description": "This server. Set automatically to the path this API is mounted under (e.g. / locally, /dronedash/api on the website)." }
  ],
  "paths": {
    "/current-meeting.json": {
      "get": {
        "summary": "The active meeting's id + name",
        "description": "Discover the current meeting so you can pass its id to the other endpoints (or just omit the id and let them default to it). Returns {\"loaded\":false} when no meeting is active.",
        "parameters": [ { "$ref": "#/components/parameters/pretty" } ],
        "responses": {
          "200": {
            "description": "Active meeting id+name, or {\"loaded\":false}.",
            "content": { "application/json": { "schema": { "$ref": "#/components/schemas/CurrentMeeting" } } }
          }
        }
      }
    },
    "/meeting.json": {
      "get": {
        "summary": "Meeting metadata only",
        "description": "A cheap poll: is a meeting loaded, and how many races/pilots, how many pending/running/ended. Returns {\"loaded\":false} when none.",
        "parameters": [ { "$ref": "#/components/parameters/meeting" }, { "$ref": "#/components/parameters/pretty" } ],
        "responses": {
          "200": { "description": "Meeting summary (or {\"loaded\":false}).", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/MeetingSummary" } } } }
        }
      }
    },
    "/state.json": {
      "get": {
        "summary": "Full meeting state",
        "description": "Every race (with results), the current race, recent + upcoming heats, and the leaderboard.",
        "parameters": [ { "$ref": "#/components/parameters/meeting" }, { "$ref": "#/components/parameters/pretty" } ],
        "responses": {
          "200": { "description": "Meeting view (or {\"waiting\":true}).", "content": { "application/json": { "schema": { "oneOf": [ { "$ref": "#/components/schemas/MeetingView" }, { "$ref": "#/components/schemas/Waiting" } ] } } } }
        }
      }
    },
    "/current.json": {
      "get": {
        "summary": "Current race only",
        "description": "The running heat, or the most-recently-started / on-deck heat when nothing is live. Pilots carry channel + timing. {\"waiting\":true} with no meeting; {\"current\":null} when nothing is current.",
        "parameters": [ { "$ref": "#/components/parameters/meeting" }, { "$ref": "#/components/parameters/pretty" } ],
        "responses": {
          "200": { "description": "The current race (or a sentinel).", "content": { "application/json": { "schema": { "oneOf": [ { "$ref": "#/components/schemas/Race" }, { "$ref": "#/components/schemas/Waiting" }, { "$ref": "#/components/schemas/CurrentNull" } ] } } } }
        }
      }
    },
    "/next.json": {
      "get": {
        "summary": "Next on-deck race",
        "parameters": [ { "$ref": "#/components/parameters/meeting" }, { "$ref": "#/components/parameters/pretty" } ],
        "responses": {
          "200": { "description": "The next race (or {\"waiting\":true} / {\"next\":null}).", "content": { "application/json": { "schema": { "oneOf": [ { "$ref": "#/components/schemas/Race" }, { "$ref": "#/components/schemas/Waiting" }, { "$ref": "#/components/schemas/NextNull" } ] } } } }
        }
      }
    },
    "/races.json": {
      "get": {
        "summary": "All races (filterable)",
        "parameters": [
          { "$ref": "#/components/parameters/meeting" },
          { "name": "state", "in": "query", "description": "Keep only these states. Repeatable or comma-list.", "schema": { "type": "string", "enum": [ "pending", "running", "ended" ] } },
          { "name": "round", "in": "query", "description": "Keep only this round.", "schema": { "type": "integer" } },
          { "name": "bracket", "in": "query", "description": "Keep only bracket (true) / non-bracket (false) heats.", "schema": { "type": "boolean" } },
          { "name": "id", "in": "query", "description": "Keep only this race id.", "schema": { "type": "string" } },
          { "$ref": "#/components/parameters/limit" },
          { "$ref": "#/components/parameters/pretty" }
        ],
        "responses": {
          "200": { "description": "Array of races, or {\"waiting\":true}.", "content": { "application/json": { "schema": { "type": "array", "items": { "$ref": "#/components/schemas/Race" } } } } }
        }
      }
    },
    "/race.json": {
      "get": {
        "summary": "A single race by id, or by round + race number",
        "description": "Address a heat by ?id=, or by ?round=N&number=M (the human 'Round N — Race M' coordinate).",
        "parameters": [
          { "$ref": "#/components/parameters/meeting" },
          { "name": "id", "in": "query", "description": "The race id. Mutually exclusive with round+number.", "schema": { "type": "string" } },
          { "name": "round", "in": "query", "description": "Round number (with number).", "schema": { "type": "integer" } },
          { "name": "number", "in": "query", "description": "Race number within the round (with round).", "schema": { "type": "integer" } },
          { "$ref": "#/components/parameters/pretty" }
        ],
        "responses": {
          "200": { "description": "The race.", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Race" } } } },
          "400": { "description": "Neither selector given.", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } },
          "404": { "description": "No matching race.", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } }
        }
      }
    },
    "/rounds.json": {
      "get": {
        "summary": "All rounds (summary)",
        "description": "Per round: number/name, per-state counts, and the race ids (no heavy bodies).",
        "parameters": [ { "$ref": "#/components/parameters/meeting" }, { "$ref": "#/components/parameters/pretty" } ],
        "responses": {
          "200": { "description": "Array of round summaries.", "content": { "application/json": { "schema": { "type": "array", "items": { "$ref": "#/components/schemas/RoundSummary" } } } } }
        }
      }
    },
    "/round.json": {
      "get": {
        "summary": "A single round with its races",
        "parameters": [
          { "$ref": "#/components/parameters/meeting" },
          { "name": "round", "in": "query", "required": true, "description": "The round number.", "schema": { "type": "integer" } },
          { "$ref": "#/components/parameters/pretty" }
        ],
        "responses": {
          "200": { "description": "The round with its races.", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/RoundDetail" } } } },
          "400": { "description": "Missing ?round.", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } },
          "404": { "description": "No such round.", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } }
        }
      }
    },
    "/board.json": {
      "get": {
        "summary": "Event-wide leaderboard",
        "description": "Aliased at /leaderboard.json.",
        "parameters": [ { "$ref": "#/components/parameters/meeting" }, { "$ref": "#/components/parameters/limit" }, { "$ref": "#/components/parameters/pretty" } ],
        "responses": { "200": { "description": "Array of standings.", "content": { "application/json": { "schema": { "type": "array", "items": { "$ref": "#/components/schemas/Standing" } } } } } }
      }
    },
    "/channels.json": {
      "get": {
        "summary": "The meeting's VTX channel set (frequency grid)",
        "description": "Every channel the meeting uses, frequency order. Lets a client show the whole spectrum incl. channels no pilot is on the current heat.",
        "parameters": [ { "$ref": "#/components/parameters/meeting" }, { "$ref": "#/components/parameters/pretty" } ],
        "responses": { "200": { "description": "Array of channels.", "content": { "application/json": { "schema": { "type": "array", "items": { "$ref": "#/components/schemas/Channel" } } } } } }
      }
    },
    "/leaderboard.json": {
      "get": {
        "summary": "Event-wide leaderboard (alias of /board.json)",
        "parameters": [ { "$ref": "#/components/parameters/meeting" }, { "$ref": "#/components/parameters/limit" }, { "$ref": "#/components/parameters/pretty" } ],
        "responses": { "200": { "description": "Array of standings.", "content": { "application/json": { "schema": { "type": "array", "items": { "$ref": "#/components/schemas/Standing" } } } } } }
      }
    },
    "/pilots.json": {
      "get": {
        "summary": "Pilots in the meeting",
        "parameters": [
          { "$ref": "#/components/parameters/meeting" },
          { "name": "q", "in": "query", "description": "Case-insensitive name substring filter.", "schema": { "type": "string" } },
          { "$ref": "#/components/parameters/limit" },
          { "$ref": "#/components/parameters/pretty" }
        ],
        "responses": { "200": { "description": "Array of standings.", "content": { "application/json": { "schema": { "type": "array", "items": { "$ref": "#/components/schemas/Standing" } } } } } }
      }
    },
    "/pilot.json": {
      "get": {
        "summary": "One pilot's meeting-wide detail",
        "parameters": [
          { "$ref": "#/components/parameters/meeting" },
          { "name": "name", "in": "query", "required": true, "description": "The pilot callsign (case-insensitive).", "schema": { "type": "string" } },
          { "$ref": "#/components/parameters/pretty" }
        ],
        "responses": {
          "200": { "description": "Pilot detail.", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/PilotView" } } } },
          "400": { "description": "Missing ?name.", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } },
          "404": { "description": "No such pilot.", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } }
        }
      }
    },
    "/events": {
      "get": {
        "summary": "Live update stream (SSE)",
        "description": "Fires on every meeting change, plus a 'ping' keepalive every 15s. Default sends 'data: tick'. ?data=current|state pushes the JSON payload itself on each change (and an initial frame).",
        "parameters": [
          { "$ref": "#/components/parameters/meeting" },
          { "name": "data", "in": "query", "description": "Omitted = 'tick'; 'current' = push current race JSON; 'state' = push full meeting JSON.", "schema": { "type": "string", "enum": [ "current", "state" ] } }
        ],
        "responses": { "200": { "description": "An event stream.", "content": { "text/event-stream": { "schema": { "type": "string" } } } } }
      }
    },
    "/current.stream": {
      "get": {
        "summary": "Current-race change stream (SSE)",
        "description": "Pushes the current race JSON ONLY when the current heat changes (different heat, or running→ended) — not on every lap. Initial frame on connect; 'ping' every 15s. Listen with: new EventSource('/current.stream').onmessage.",
        "parameters": [ { "$ref": "#/components/parameters/meeting" } ],
        "responses": { "200": { "description": "An event stream of current-race frames.", "content": { "text/event-stream": { "schema": { "type": "string" } } } } }
      }
    }
  },
  "components": {
    "parameters": {
      "meeting": { "name": "meeting", "in": "query", "description": "Optional meeting/event id (alias: ?event=). Omit to use the current/active meeting — so the same call works local or remote.", "schema": { "type": "string" } },
      "pretty": { "name": "pretty", "in": "query", "description": "Set 1/true for indented JSON.", "schema": { "type": "boolean" } },
      "limit": { "name": "limit", "in": "query", "description": "Cap the number of items (0/absent = no cap).", "schema": { "type": "integer", "minimum": 0 } }
    },
    "schemas": {
      "Waiting": { "type": "object", "description": "No meeting loaded yet.", "properties": { "waiting": { "type": "boolean", "example": true } } },
      "CurrentNull": { "type": "object", "properties": { "current": { "type": "null" } } },
      "NextNull": { "type": "object", "properties": { "next": { "type": "null" } } },
      "Error": { "type": "object", "properties": { "error": { "type": "string" } } },
      "CurrentMeeting": {
        "type": "object",
        "description": "The active meeting, from /current-meeting.json.",
        "properties": {
          "loaded": { "type": "boolean", "description": "False (alone) when no meeting is active." },
          "MeetingID": { "type": "string" },
          "Name": { "type": "string" }
        }
      },
      "MeetingSummary": {
        "type": "object",
        "properties": {
          "loaded": { "type": "boolean" },
          "MeetingID": { "type": "string" },
          "Name": { "type": "string" },
          "IsTT": { "type": "boolean" },
          "Window": { "type": "integer" },
          "RaceCount": { "type": "integer" },
          "Pending": { "type": "integer" },
          "Running": { "type": "integer" },
          "Ended": { "type": "integer" },
          "UpcomingTotal": { "type": "integer" },
          "PilotCount": { "type": "integer" },
          "HasCurrent": { "type": "boolean" }
        }
      },
      "PilotView": {
        "type": "object",
        "properties": {
          "Name": { "type": "string" },
          "Standing": { "oneOf": [ { "$ref": "#/components/schemas/Standing" }, { "type": "null" } ] },
          "Races": { "type": "array", "items": { "$ref": "#/components/schemas/Race" } }
        }
      },
      "RoundSummary": {
        "type": "object",
        "properties": {
          "Round": { "type": "integer" },
          "RoundName": { "type": "string" },
          "RaceCount": { "type": "integer" },
          "Pending": { "type": "integer" },
          "Running": { "type": "integer" },
          "Ended": { "type": "integer" },
          "RaceIDs": { "type": "array", "items": { "type": "string" } }
        }
      },
      "RoundDetail": {
        "type": "object",
        "properties": {
          "Round": { "type": "integer" },
          "RoundName": { "type": "string" },
          "RaceCount": { "type": "integer" },
          "Pending": { "type": "integer" },
          "Running": { "type": "integer" },
          "Ended": { "type": "integer" },
          "Races": { "type": "array", "items": { "$ref": "#/components/schemas/Race" } }
        }
      },
      "MeetingView": {
        "type": "object",
        "properties": {
          "MeetingID": { "type": "string" },
          "Name": { "type": "string" },
          "IsTT": { "type": "boolean" },
          "Window": { "type": "integer" },
          "Races": { "type": "array", "items": { "$ref": "#/components/schemas/Race" } },
          "Current": { "oneOf": [ { "$ref": "#/components/schemas/Race" }, { "type": "null" } ] },
          "Recent": { "type": "array", "items": { "$ref": "#/components/schemas/Race" } },
          "Upcoming": { "type": "array", "items": { "$ref": "#/components/schemas/Race" } },
          "UpcomingTotal": { "type": "integer" },
          "Board": { "type": "array", "items": { "$ref": "#/components/schemas/Standing" } }
        }
      },
      "Race": {
        "type": "object",
        "properties": {
          "ID": { "type": "string" },
          "Round": { "type": "integer" },
          "Number": { "type": "integer", "description": "Race number within the round." },
          "RoundName": { "type": "string" },
          "Label": { "type": "string" },
          "IsTT": { "type": "boolean" },
          "TypeLabel": { "type": "string", "description": "The heat's own effective type — \"Time Trial\" or \"Race\" — independent of the meeting-wide TT display enrichment of IsTT." },
          "ConsecWindow": { "type": "integer" },
          "State": { "type": "string", "enum": [ "pending", "running", "ended" ] },
          "StartMs": { "type": "integer", "format": "int64" },
          "TargetLaps": { "type": "integer" },
          "Bracket": { "type": "boolean" },
          "ExtID": { "type": "integer" },
          "Pilots": { "type": "array", "items": { "$ref": "#/components/schemas/Pilot" } }
        }
      },
      "Pilot": {
        "type": "object",
        "properties": {
          "Name": { "type": "string" },
          "ChannelLabel": { "type": "string", "description": "VTX channel, e.g. R1." },
          "ChannelColor": { "type": "string", "description": "Channel colour hex." },
          "Holeshot": { "type": "number", "format": "double" },
          "Laps": { "type": "array", "items": { "type": "number", "format": "double" } },
          "DispLaps": { "type": "array", "items": { "type": "number", "format": "double" } },
          "Total": { "type": "number", "format": "double" },
          "CountedLaps": { "type": "integer" },
          "Position": { "type": "integer" },
          "DNF": { "type": "boolean" },
          "BestLap": { "type": "number", "format": "double" },
          "BestConsec": { "type": "number", "format": "double" },
          "PBConsec": { "type": "number", "format": "double" }
        }
      },
      "Standing": {
        "type": "object",
        "properties": {
          "Pos": { "type": "integer" },
          "Name": { "type": "string" },
          "ChannelLabel": { "type": "string" },
          "ChannelColor": { "type": "string" },
          "Laps": { "type": "integer" },
          "Holeshot": { "type": "number", "format": "double" },
          "BestLap": { "type": "number", "format": "double" },
          "BestConsec": { "type": "number", "format": "double" },
          "FastestRace": { "type": "number", "format": "double" },
          "NextStatus": { "type": "string" }
        }
      },
      "Channel": {
        "type": "object",
        "properties": {
          "Label": { "type": "string", "description": "e.g. R1 (DisplayName, else ShortBand+Number)." },
          "Band": { "type": "string" },
          "ShortBand": { "type": "string" },
          "Number": { "type": "integer" },
          "Frequency": { "type": "integer", "description": "MHz." },
          "Color": { "type": "string", "description": "Channel colour hex; may be empty." }
        }
      }
    }
  }
}`
