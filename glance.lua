-- Glance at…: another machine's screen in a floating window over this one,
-- from Rex's palette. rex-glance asks for the password there (typing hidden)
-- when none is saved. Loaded from init.lua with  pcall(require, "glance")
--
--   require("glance").setup{ machines = { { name = "This Mac", host = "localhost" }, "linuxbox" },
--                            bin = "~/.local/bin/rex-glance", key = "ctrl+alt+cmd+g" }

local M = {}

local home = os.getenv("HOME") or ""
local config = {
  -- An ssh or tailnet name, or { name = …, host = … }. Set yours with setup.
  machines = { { name = "This Mac", host = "localhost" } },
  bin = home .. "/.local/bin/rex-glance",
  -- Rex apps on these platforms ask which machine in the window, not the
  -- palette: Rex asks palette questions on every connected app and the first
  -- answer wins, so an app that can't show them (the iPad's, for now) would
  -- have its question answered on another device. Set to {} once yours can.
  ask_in_window = { ios = true },
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
  if ctx and ctx.session_id and not managed(ctx.session_id) then
    local r, err = rex.session.new_layer{ session_id = ctx.session_id, bounds = { 0.05, 0.05, 0.9, 0.9 },
      focus = true, layout = { block = block } }
    if err then error(err, 0) end
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
  local s, err = rex.call("session.create", { label = label,
    initial_windows = { { window_label = label, layout = { block = block } } } })
  if s == nil then error(err, 0) end
  rex.client.queue("session.select", { session_id = s.session_id })
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
