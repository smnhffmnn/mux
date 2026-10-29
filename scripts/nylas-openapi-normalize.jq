# Normalization shared by nylas-openapi-snapshot.sh and nylas-openapi-report.sh:
# drop prose (string-valued "description") and examples, keep structure.
walk(
  if type == "object" then
    (if (.description? | type) == "string" then del(.description) else . end)
    | del(.example, .examples)
  else . end
)
