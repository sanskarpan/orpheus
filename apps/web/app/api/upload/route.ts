import { NextResponse } from "next/server";
import { orpheus, OrpheusError } from "@/lib/orpheus";
import { getApiKey } from "@/lib/session";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * Full upload proxy: browser POSTs the file here (multipart), the Next server
 * runs the presigned-multipart dance against MinIO/S3 and returns the created
 * Artifact. Doing the part PUTs server-side means the browser never needs S3
 * CORS and the API key stays server-side.
 */
export async function POST(req: Request) {
  const key = await getApiKey();
  if (!key) return NextResponse.json({ error: "Not authenticated" }, { status: 401 });

  let form: FormData;
  try {
    form = await req.formData();
  } catch {
    return NextResponse.json({ error: "Expected multipart form data" }, { status: 400 });
  }

  const file = form.get("file");
  if (!(file instanceof File)) {
    return NextResponse.json({ error: "Missing file" }, { status: 400 });
  }

  const buf = Buffer.from(await file.arrayBuffer());
  if (buf.length === 0) {
    return NextResponse.json({ error: "That file is empty." }, { status: 400 });
  }
  const contentType = file.type || "application/octet-stream";

  try {
    // 1. Initiate — returns presigned part URLs + part size.
    const session = await orpheus.createUpload({
      filename: file.name,
      content_type: contentType,
      size_bytes: buf.length,
    });

    // 2. PUT each part to its presigned URL (server-side; no browser CORS).
    const partSize = session.part_size > 0 ? session.part_size : buf.length;
    const sorted = [...(session.parts ?? [])].sort((a, b) => a.part_number - b.part_number);
    if (sorted.length === 0) {
      return NextResponse.json({ error: "Storage did not return any upload targets." }, { status: 502 });
    }
    const completed: { part_number: number; etag: string }[] = [];

    for (const part of sorted) {
      const start = (part.part_number - 1) * partSize;
      const chunk = buf.subarray(start, Math.min(start + partSize, buf.length));
      const put = await fetch(part.url, {
        method: "PUT",
        body: chunk,
        headers: { "Content-Type": contentType },
      });
      if (!put.ok) {
        const body = await put.text();
        return NextResponse.json(
          { error: `Storage rejected part ${part.part_number} (${put.status})`, detail: body.slice(0, 200) },
          { status: 502 },
        );
      }
      const etag = (put.headers.get("etag") ?? "").replace(/"/g, "");
      completed.push({ part_number: part.part_number, etag });
    }

    // 3. Complete — returns the Artifact.
    const artifact = await orpheus.completeUpload(session.id, completed);
    return NextResponse.json({ artifact });
  } catch (e) {
    if (e instanceof OrpheusError) {
      return NextResponse.json({ error: e.detail || e.message, kind: e.kind }, { status: e.status });
    }
    return NextResponse.json({ error: "Upload failed", detail: String(e) }, { status: 500 });
  }
}
