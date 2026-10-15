export function createFixture() {
  return {languageModel(modelId) { return {
    specificationVersion: 'v3', provider: 'fixture', modelId,
    supportedUrls: {},
    async doGenerate() { return {content: [{type: 'text', text: 'Fixture run'}], finishReason: {unified: 'stop', raw: 'stop'}, usage: {inputTokens: {total: 1}, outputTokens: {total: 1}}, warnings: []} },
    async doStream(options) {
      if (process.env.FIXTURE_MODE !== 'normal') {
        const statusCode = process.env.FIXTURE_MODE === 'limit' ? 429 : 401;
        const error = new Error(statusCode === 429 ? 'Usage limit reached: quota exhausted' : 'Invalid API key');
        error.name = 'AI_APICallError';
        error[Symbol.for('vercel.ai.error.AI_APICallError')] = true;
        error.statusCode = statusCode; error.isRetryable = false; error.url = 'https://fixture.invalid/v1/chat/completions'; error.requestBodyValues = {}; error.responseBody = JSON.stringify({error: {message: error.message}});
        throw error;
      }
      const done = options.prompt.some(m => m.role === 'tool');
      const chunks = [{type: 'stream-start', warnings: []}];
      if (done) chunks.push({type: 'text-start', id: 'text-1'}, {type: 'text-delta', id: 'text-1', delta: 'The final answer text.'}, {type: 'text-end', id: 'text-1'});
      else chunks.push({type: 'tool-call', toolCallId: 'call_read', toolName: 'read', input: JSON.stringify({filePath: process.env.FIXTURE_FILE})});
      chunks.push({type: 'finish', finishReason: {unified: done ? 'stop' : 'tool-calls', raw: done ? 'stop' : 'tool_calls'}, usage: {inputTokens: {total: 10, noCache: 10, cacheRead: 0, cacheWrite: 0}, outputTokens: {total: 5, text: 5, reasoning: 0}}});
      return {stream: new ReadableStream({start(controller) { for (const c of chunks) controller.enqueue(c); controller.close(); }})};
    }
  }}};
}
