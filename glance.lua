-- Glance at…: another machine's screen, from Rex's palette (rex-glance).
--
-- Opens a floating window over the tab you're in; in tr3x's T3 Code session,
-- or where no floater can go, a session of its own that closes when you quit.
-- Which machine is asked in the palette (rex.ui.pick); for a platform listed
-- in ask_in_window, or when the palette can't ask, the window opens at once
-- and asks there instead. rex-glance asks for the
-- password in the window (typing hidden) when none is saved.
--
-- Loaded from init.lua:
--
--   pcall(function()
--     require("glance").setup{
--       machines      = { { name = "This Mac", host = "localhost" }, "linuxbox" },
--       bin           = "~/.local/bin/rex-glance",   -- the default
--       ask_in_window = {},                          -- the default; e.g. { ios = true } for an app that can't ask
--       key           = "ctrl+alt+cmd+g",            -- optional
--     }
--   end)

local M = {}

local home = os.getenv("HOME") or ""
local config = {
  -- An ssh or tailnet name, or { name = …, host = … }. Set yours with setup.
  machines = { { name = "This Mac", host = "localhost" } },
  bin = home .. "/.local/bin/rex-glance",
  -- Rex apps on these platforms ask which machine in the window, not the
  -- palette: Rex asks palette questions on every connected app and the first
  -- answer wins, so an app that can't show them would have its question
  -- answered on another device. Every Rex app can since iOS 0.1.1 (build
  -- 2127), so none by default; list a platform here to keep it in the window.
  ask_in_window = {},
}
local bound_key = nil

-- The floater's command: rex-glance, and if it stops with an error, the
-- error stays on screen until Return.
local WRAP = [[
"$@"
s=$?
if [ $s -ne 0 ]; then printf '\nPress Return to close. '; read -r _; fi
exit $s
]]

-- The same, for a Glance in a session of its own: when it ends, the session
-- goes too, rather than staying behind with no tabs.
--
-- Rex server preview.24 stopped setting REX_SESSION in new panes, so when
-- it's empty the session comes from the pane's block (REX_BLOCK).
local WRAP_OWN_HEAD = [[
r=rex; command -v rex >/dev/null 2>&1 || r="$REX_BIN_DIR/rex"
if [ -z "$REX_SESSION" ] && [ -n "$REX_BLOCK" ]; then
  REX_SESSION=$("$r" block inspect "$REX_BLOCK" 2>/dev/null | awk '/^session:/ { print $2; exit }')
fi
]]
local WRAP_OWN = [[
"$@"
s=$?
if [ $s -ne 0 ]; then printf '\nPress Return to close. '; read -r _; fi
[ -n "$REX_SESSION" ] && "$r" kill "$REX_SESSION" >/dev/null 2>&1
exit $s
]]

-- find_block looks through what the server returned for a block: the first
-- block id, or, with a label, the block that has it.
local function find_block(t, label, depth)
  depth = depth or 0
  if type(t) ~= "table" or depth > 12 then return nil end
  if label == nil then
    if type(t.block_ids) == "table" and t.block_ids[1] then return t.block_ids[1] end
    if t.block_id and t.flavor then return t.block_id end
  elseif t.block_id and t.label == label then
    return t.block_id
  end
  for _, v in pairs(t) do
    local found = find_block(v, label, depth + 1)
    if found then return found end
  end
  return nil
end

-- managed says whether the session belongs to a tool that manages it: tr3x's
-- T3 Code session ("T3 Code", or "T3 Code · <machine>" with one per machine).
local function managed(session_id)
  local ok, r = pcall(rex.call, "session.list", {})
  if not ok or type(r) ~= "table" then return false end
  for _, s in ipairs(r.sessions or r) do
    if s.session_id == session_id then
      return type(s.label) == "string" and s.label:sub(1, 7) == "T3 Code"
    end
  end
  return false
end

-- caller_platform is the platform of the Rex app that ran the action.
local function caller_platform(ctx)
  if not (ctx and ctx.client_id) then return nil end
  local ok, r = pcall(rex.call, "client.list", {})
  if not ok or type(r) ~= "table" then return nil end
  for _, c in ipairs(r.clients or r) do
    if c.client_id == ctx.client_id then return c.info and c.info.platform end
  end
  return nil
end

-- own_session opens Glance in a new session and switches to it. The
-- session closes itself when Glance ends.
--
-- Rex for iOS learns about a new session a moment after it's created, so a
-- Go to Session queued straight away fails with "The session isn't available
-- right now". For those apps, the new pane asks the app to go there itself,
-- trying every half second for up to 3 seconds (the first try usually works).
local late_select = { ios = true }

local function own_session(block, label, ctx)
  local platform = caller_platform(ctx) or ""
  local client = ctx and ctx.client_id
  local delayed = late_select[platform] == true and type(client) == "string"
    and client:match("^[%w:%-]+$") ~= nil
  local script = WRAP_OWN_HEAD .. WRAP_OWN
  if delayed then
    script = WRAP_OWN_HEAD .. string.format([[
(i=0
 while [ $i -lt 6 ]; do
   sleep 0.5; i=$((i+1))
   "$r" do -C %s session.select session_id="$REX_SESSION" >/dev/null 2>&1 && break
 done) &
]], client) .. WRAP_OWN
  end
  block.options.command[3] = script
  local s, err = rex.call("session.create", { label = label,
    initial_windows = { { window_label = label, layout = { block = block } } } })
  if s == nil then error(err, 0) end
  if not delayed then
    rex.client.queue("session.select", { session_id = s.session_id })
  end
end

local function glance(ctx)
  local items = {}
  for _, m in ipairs(config.machines) do
    if type(m) == "string" then m = { name = m, host = m } end
    items[#items + 1] = { id = m.host, title = m.name, subtitle = m.host ~= m.name and m.host or nil, icon = "server" }
  end
  -- Which machine: in the palette, or, for an app that can't show it (or
  -- when the palette can't ask), in the window, which opens straight away.
  local host, label = nil, "Glance"
  local in_window = config.ask_in_window[caller_platform(ctx) or ""] == true
  if not in_window then
    local why
    host, why = rex.ui.pick{ title = "Glance at", placeholder = "Which machine", confirm = "Open", items = items }
    if host == nil then
      if why ~= "unsupported" and why ~= "failed" then return end
      in_window = true
    end
  end
  local command = { "/bin/sh", "-c", WRAP, "glance", config.bin, "look" }
  if in_window then
    command[#command + 1] = "--choose"
    for _, it in ipairs(items) do command[#command + 1] = it.title .. "=" .. it.id end
  else
    command[#command + 1] = host
    for _, it in ipairs(items) do if it.id == host then label = "Glance: " .. it.title end end
  end
  local block = { flavor = "com.superlogical.terminal.shell", label = label,
    options = { shell = "none", login = false, command = command } }
  -- A floater over the window you're in, unless that window is another
  -- tool's to manage: tr3x keeps the T3 Code session in step with T3 and
  -- moves focus back to its own panes, so there Glance gets a session of its
  -- own instead.
  -- A session with no tabs left has no window to float over: then, and on
  -- any other failure, Glance gets a session of its own.
  if ctx and ctx.session_id and not managed(ctx.session_id) then
    local r, err = rex.session.new_layer{ session_id = ctx.session_id, bounds = { 0.05, 0.05, 0.9, 0.9 },
      focus = true, layout = { block = block } }
    if err then
      rex.log("info", "glance: no floater here (" .. tostring(err) .. "); opening a session of its own")
      return own_session(block, label, ctx)
    end
    rex.session.disown_block{ session_id = ctx.session_id }
    -- focus = true should be enough, but the palette closing can hand focus
    -- back to where it was, so focus the new block once more by its id.
    local id = find_block(r)
    if id == nil then
      local view = rex.session.view{ session_id = ctx.session_id }
      id = find_block(view, block.label)
    end
    if id then
      rex.session.focus_block{ session_id = ctx.session_id, block_id = id }
    else
      rex.log("warn", "glance: couldn't find the new block to focus; new_layer returned " .. rex.json.encode(r or {}))
    end
    return
  end
  own_session(block, label, ctx)
end

rex.action{
  name = "glance",
  title = "Glance at…",
  description = "Another machine's screen in a floating window, to do something by hand",
  category = "Machines",
  keywords = "glance screen share vnc remote desktop view mac linux",
  run = glance,
}

function M.setup(opts)
  opts = opts or {}
  if opts.machines ~= nil then config.machines = opts.machines end
  if opts.bin ~= nil then config.bin = opts.bin end
  if opts.ask_in_window ~= nil then config.ask_in_window = opts.ask_in_window end
  if opts.key ~= nil then
    if bound_key then pcall(rex.unbind, bound_key) end
    bound_key = nil
    if opts.key then rex.bind(opts.key, "glance"); bound_key = opts.key end
  end
  return M
end

return M
