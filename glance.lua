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

local function glance(ctx)
  local items = {}
  for _, m in ipairs(config.machines) do
    if type(m) == "string" then m = { name = m, host = m } end
    items[#items + 1] = { id = m.host, title = m.name, subtitle = m.host ~= m.name and m.host or nil, icon = "terminal" }
  end
  local host = rex.ui.pick{ title = "Glance at", placeholder = "Which machine", confirm = "Open", items = items }
  if host == nil then return end
  local name = host
  for _, it in ipairs(items) do if it.id == host then name = it.title end end
  local block = { flavor = "com.superlogical.terminal.shell", label = "Glance: " .. name,
    options = { shell = "none", login = false,
      command = { "/bin/sh", "-c", WRAP, "glance", config.bin, "look", host } } }
  if ctx and ctx.session_id then
    local _, err = rex.session.new_layer{ session_id = ctx.session_id, bounds = { 0.05, 0.05, 0.9, 0.9 },
      focus = true, layout = { block = block } }
    rex.session.disown_block{ session_id = ctx.session_id }
    if err then error(err, 0) end
    return
  end
  local s, err = rex.call("session.create", { label = "Glance: " .. name,
    initial_windows = { { window_label = name, layout = { block = block } } } })
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
  if opts.key ~= nil then
    if bound_key then pcall(rex.unbind, bound_key) end
    bound_key = nil
    if opts.key then rex.bind(opts.key, "glance"); bound_key = opts.key end
  end
  return M
end

return M
