export function parseQuery(search) {
  const out = {};
  for (const pair of search.replace(/^\?/, "").split("&")) {
    if (!pair) continue;
    const [key, value] = pair.split("=");
    out[decodeURIComponent(key)] = decodeURIComponent(value);
  }
  return out;
}
