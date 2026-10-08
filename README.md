# Philips Hue for Rubi

A [Rubi](https://github.com/Deikus-LXXVII/rubi) plugin that lets your AI agent see and control your
Philips Hue lights:

- list your rooms and zones with their lights: on or off, brightness, color (a rough name and hex, or a
  white with its kelvin) and lights the bridge can't reach;
- switch a room, zone or light on or off, dim it, and change its color: a name ("warm white", "daylight",
  "blue"), a hex color (`#ff8800`) or a white color temperature (`2700K`), with an optional fade;
- list the scenes of each room and zone, and recall one;
- switch off every light at once;
- connect several bridges, one account each. Your agent picks one by its name; otherwise it uses the
  default (the first one you connected).

Your agent names things the way you do: "the kitchen", "living room lights", "bedroom lamp". Names are
matched without regard to case, and small typos are forgiven. When a name could mean several lights (two
rooms each have a "Ceiling"), the agent is told which ones and adds the room ("Kitchen Ceiling").

Lights are low-risk, so nothing needs your approval by default. You can raise the levels in Rubi's
settings; then each change waits for your approval and runs exactly as shown.

## How it reaches your lights

Rubi runs on your agent's machine, not at home, so it can't reach your home network. **Rubi Home** can:
it is a small helper on a computer at home (for example a Mac that stays on). Rubi calls it through relays
with end-to-end encryption, and Rubi Home passes the requests to the bridge on your network.

This plugin connects to nothing itself: it has no internet access of its own (no egress) and talks to the
bridge only through Rubi Home. Rubi Home only passes requests to the bridge's light API (`/clip/v2/resource`,
read and change, never delete), only to the bridge you paired, and checks the bridge's certificate on every
connection.

## Install

First install Rubi Home on a computer at home, on the same network as your Hue Bridge, and pair it in the
Rubi panel under *Settings > Rubi Home*.

Then ask your agent: "Connect Philips Hue through Rubi." It installs the plugin from the Rubi store (you
approve what it can do), then sends you a setup link. There:

1. Press *Connect Philips Hue*. Rubi Home looks for your bridge. If you have several Rubi Home computers,
   pick the one at the bridge's home; if it finds several bridges, pick one.
2. Press the round link button on top of your Hue Bridge, then press *Continue*. If you were too slow,
   setup says so; press the button again and *Continue*.

The bridge gives Rubi an application key. Rubi keeps it encrypted; your agent never sees it. The account
is named after the bridge, for example "Hue Bridge (5 rooms)".

To add another bridge, open *Settings* in the Rubi panel and press *Add account* at Philips Hue. Setting
up the same bridge again replaces its key.

## Tools

| Tool | What it does |
|---|---|
| `hue_lights(room?)` | Rooms with their lights, zones with their lights' names, and lights in no room |
| `hue_scenes(room?)` | Scenes of each room and zone, and which one is active |
| `hue_set(target, on?, brightness?, color?, transition?)` | Change a room, zone or light ("all" for every light) |
| `hue_scene(name, room?, transition?)` | Recall a scene |
| `hue_all_off(transition?)` | Switch off every light |
| `hue_accounts` | The connected bridges |

Every tool but `hue_accounts` takes an optional `account` (which bridge).

## Permissions

| | |
|---|---|
| Asks for | nothing; you press the bridge's link button during setup |
| See your lights, rooms, zones and scenes | no approval by default |
| Switch, dim and color your lights and recall scenes | no approval by default |
| Connects to | nothing; it uses Rubi Home (Philips Hue) |

## Good to know

- **Rubi Home off.** If the computer at home is off or asleep, your agent is told "Rubi Home didn't
  answer".
- **The bridge moved.** If your router gives the bridge a new address, Rubi Home finds it again by its id;
  the plugin remembers the new address.
- **Not too fast.** The room and light map is cached for 30 seconds, so finding a light by name doesn't cost
  a round trip each time; any change refreshes it. Rubi Home keeps requests to the bridge 100 ms apart, and
  the plugin keeps room and zone changes a second apart, as Signify asks.
- **Key revoked.** If the bridge is reset, or Rubi is removed in the Hue app, set Philips Hue up again.

## Development

```bash
go test -race ./...
go build -o hue . && ./hue --manifest
```

Tests run against a fake Rubi and a fake Rubi Home that answers with canned CLIP v2 data; they never touch
the network or a real bridge. To develop against a local checkout of Rubi, use a workspace (not committed):

```bash
go work init . ../RubiProject
```

## Releasing

Tag `vX.Y.Z`. The release workflow tests, builds for linux/amd64, linux/arm64 and darwin/arm64, writes
`rubi-plugin.json`, signs `SHA256SUMS` with the plugin key (`PLUGIN_SIGNING_KEY`; public half in
`publisher-key.pub.pem`), attests the build, and publishes the release. It prints the SHA-256 of
`SHA256SUMS` for the Rubi store catalog.

## Credits

Field names follow the Hue CLIP v2 API, as described by Signify and by the
[openhue-api](https://github.com/openhue/openhue-api) specification. Philips Hue is a trademark of Signify.
This plugin is not affiliated with Signify.

## License

[MIT](LICENSE)
