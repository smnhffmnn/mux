# Normalization shared by the OpenAPI snapshot and report scripts (nylas, bunq):
# drop prose (string-valued "description") and examples, keep structure.
walk(
  if type == "object" then
    (if (.description? | type) == "string" then del(.description) else . end)
    | del(.example, .examples)
  else . end
)
