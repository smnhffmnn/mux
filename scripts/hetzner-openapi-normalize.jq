# Normalization shared by hetzner-openapi-snapshot.sh and hetzner-openapi-report.sh:
# drop prose (string-valued "description", "x-summary"), examples, code samples
# and enum prose, keep structure. Property *names* called "description" are
# objects and survive; only string-valued "description" keys are prose.
walk(
  if type == "object" then
    (if (.description? | type) == "string" then del(.description) else . end)
    | del(.example, .examples, ."x-codeSamples", ."x-enumDescriptions", ."x-summary")
  else . end
)
