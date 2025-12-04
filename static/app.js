// CodeMirror 6 imports from CDN
// Use @6 without specific versions to let esm.sh deduplicate dependencies
import { EditorView, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection, keymap } from 'https://esm.sh/@codemirror/view@6';
import { EditorState, Prec } from 'https://esm.sh/@codemirror/state@6';
import { defaultKeymap, history, historyKeymap } from 'https://esm.sh/@codemirror/commands@6';
import { syntaxHighlighting, defaultHighlightStyle, bracketMatching } from 'https://esm.sh/@codemirror/language@6';
import { closeBrackets, autocompletion, closeBracketsKeymap, completionKeymap, startCompletion, snippetCompletion } from 'https://esm.sh/@codemirror/autocomplete@6';
import { highlightSelectionMatches } from 'https://esm.sh/@codemirror/search@6';
import { python } from 'https://esm.sh/@codemirror/lang-python@6';
import { cpp } from 'https://esm.sh/@codemirror/lang-cpp@6';
import { oneDark } from 'https://esm.sh/@codemirror/theme-one-dark@6';
import { linter, lintGutter } from 'https://esm.sh/@codemirror/lint@6';

// Basic setup - combining extensions manually (without autocompletion - added later with LSP)
const basicSetup = [
    lineNumbers(),
    highlightActiveLineGutter(),
    highlightActiveLine(),
    history(),
    drawSelection(),
    syntaxHighlighting(defaultHighlightStyle),
    bracketMatching(),
    closeBrackets(),
    highlightSelectionMatches(),
    keymap.of([
        ...closeBracketsKeymap,
        ...defaultKeymap,
        ...historyKeymap,
        ...completionKeymap
    ])
];

// WebSocket connection
let ws = null;
let editor = null;
let currentFilePath = null;
let isApplyingRemoteChange = false;

// Initialize WebSocket
function connectWebSocket() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(`${protocol}//${window.location.host}/ws`);

    ws.onopen = () => {
        console.log('WebSocket connected');
        showStatus('Connected to server', 'success');
    };

    ws.onmessage = (event) => {
        const message = JSON.parse(event.data);
        handleServerMessage(message);
    };

    ws.onerror = (error) => {
        console.error('WebSocket error:', error);
        showStatus('WebSocket error', 'error');
    };

    ws.onclose = () => {
        console.log('WebSocket closed');
        showStatus('Disconnected from server', 'error');
        setTimeout(connectWebSocket, 2000);
    };
}

// Handle messages from server
function handleServerMessage(message) {
    switch (message.type) {
        case 'file_opened':
            loadFileContent(message.payload.path, message.payload.content);
            showStatus(`Opened: ${message.payload.path}`, 'success');
            break;

        case 'file_saved':
            showStatus('File saved successfully', 'success');
            break;

        case 'lsp_configured':
            showStatus('LSP configured successfully', 'success');
            break;

        case 'lsp_notification':
            handleLSPNotification(message.payload);
            break;

        case 'lsp_response':
            handleLSPResponse(message.payload);
            break;

        case 'error':
            showStatus(message.payload.message, 'error');
            break;

        default:
            console.log('Unknown message type:', message.type);
    }
}

// Language detection
function getLanguageExtension(filePath) {
    if (filePath.endsWith('.py')) return python();
    if (filePath.endsWith('.cpp') || filePath.endsWith('.cc') ||
        filePath.endsWith('.cxx') || filePath.endsWith('.c') ||
        filePath.endsWith('.h') || filePath.endsWith('.hpp')) {
        return cpp();
    }
    return [];
}

// LSP diagnostics storage
let currentDiagnostics = [];
window.currentDiagnostics = currentDiagnostics;  // Expose for tests

// LSP completion tracking
let completionRequestId = 1000;  // Start at 1000 to avoid conflicts with LSP initialize
let pendingCompletionRequests = new Map();

// Create CodeMirror editor
function createEditor(content = '', filePath = '') {
    console.log('DEBUG: createEditor called with filePath:', filePath);
    const container = document.getElementById('editor-container');
    container.innerHTML = '';

    const languageExtension = getLanguageExtension(filePath);
    console.log('DEBUG: Language extension:', languageExtension);

    // Create LSP linter inside the function to avoid state instance conflicts
    const lspLinter = linter((view) => {
        return currentDiagnostics;
    });

    // LSP-based autocompletion source
    const lspCompletionSource = async (context) => {
        console.log('lspCompletionSource called, explicit:', context.explicit);

        // Only trigger if we have a file open and LSP is configured
        if (!currentFilePath || !ws || ws.readyState !== WebSocket.OPEN) {
            console.log('No file or WS not ready');
            return null;
        }

        // Check if we have at least 3 characters typed (unless explicitly triggered with Ctrl+L)
        if (!context.explicit) {
            const word = context.matchBefore(/[\w:.\->]*/);
            if (!word || word.text.length < 3) {
                console.log('Word too short, skipping completion');
                return null;
            }
        }

        const pos = context.pos;
        const doc = context.state.doc;
        const lspPos = offsetToPosition(doc, pos);

        console.log('Requesting completion at position:', lspPos);

        // Create a promise that will be resolved when we get the response
        const requestId = ++completionRequestId;
        const completionPromise = new Promise((resolve) => {
            pendingCompletionRequests.set(requestId, resolve);
        });

        // Send completion request
        ws.send(JSON.stringify({
            type: 'lsp_request',
            payload: {
                id: requestId,
                method: 'textDocument/completion',
                params: {
                    textDocument: {
                        uri: 'file://' + currentFilePath
                    },
                    position: lspPos
                }
            }
        }));

        // Wait for response with timeout
        const timeoutPromise = new Promise((resolve) => {
            setTimeout(() => {
                pendingCompletionRequests.delete(requestId);
                console.log('Completion request timed out');
                resolve(null);
            }, 3000);
        });

        const result = await Promise.race([completionPromise, timeoutPromise]);

        console.log('Got completion result:', result);

        if (!result || !result.items || result.items.length === 0) {
            console.log('No completion items');
            return null;
        }

        // Find the start of the word being completed
        const word = context.matchBefore(/[\w:.\->]*/);
        const from = word ? word.from : pos;

        // Convert LSP completion items to CodeMirror format
        const options = result.items.map(item => {
            let label = item.label;
            let insertText = item.insertText || item.label;

            // Handle text edits if present (LSP provides the exact range and text)
            if (item.textEdit) {
                insertText = item.textEdit.newText;
            }

            // If it's a snippet (insertTextFormat === 2), use snippetCompletion
            if (item.insertTextFormat === 2) {
                return snippetCompletion(insertText, {
                    label: label,
                    type: item.kind ? getLSPCompletionKind(item.kind) : 'text',
                    detail: item.detail || ''
                });
            }

            // Otherwise, plain text completion
            return {
                label: label,
                apply: insertText,
                type: item.kind ? getLSPCompletionKind(item.kind) : 'text',
                detail: item.detail || ''
            };
        });

        return {
            from: from,  // Start from beginning of word, not cursor position
            options: options,
            validFor: /^[\w:.\->]*$/  // Allow word chars, colons, dots, arrows
        };
    };

    const updateListener = EditorView.updateListener.of((update) => {
        if (update.docChanged && !isApplyingRemoteChange) {
            sendDeltas(update);
        }
    });

    console.log('DEBUG: Creating EditorState...');
    const state = EditorState.create({
        doc: content,
        extensions: [
            basicSetup,
            languageExtension,
            oneDark,
            updateListener,
            lintGutter(),
            lspLinter,
            autocompletion({
                override: [lspCompletionSource],
                activateOnTyping: true,
                defaultKeymap: true,
                activateOnCompletion: () => true,  // Keep autocomplete open
                // Explicitly trigger on common C++ completion characters
                closeOnBlur: true,
                interactionDelay: 75  // Small delay to batch rapid typing
            }),
            // Add custom keybindings with high priority
            Prec.highest(keymap.of([
                { key: 'Ctrl-l', run: startCompletion },
                { key: 'Ctrl-Space', run: startCompletion }  // Standard autocomplete shortcut
            ]))
        ],
    });

    console.log('DEBUG: Creating EditorView...');
    editor = new EditorView({
        state,
        parent: container,
    });
    window.editor = editor;  // Expose to window for tests
    console.log('DEBUG: Editor created successfully');
}

// Send deltas to server
function sendDeltas(update) {
    update.changes.iterChanges((fromA, toA, fromB, toB, inserted) => {
        const delta = {
            type: 'delta',
            payload: {
                fromPos: fromA,
                toPos: toA,
                insert: inserted.toString(),
            },
        };

        if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify(delta));
        }
    });
}

// Load file content into editor
function loadFileContent(path, content) {
    currentFilePath = path;
    document.getElementById('current-file').textContent = path;
    createEditor(content, path);
}

// Open file from UI
window.openFileFromUI = (path) => {
    console.log('DEBUG: openFileFromUI called with path:', path);
    console.log('DEBUG: ws exists?', !!ws);
    console.log('DEBUG: ws readyState:', ws ? ws.readyState : 'NO WS');
    if (ws && ws.readyState === WebSocket.OPEN) {
        console.log('DEBUG: Sending open_file message');
        ws.send(JSON.stringify({
            type: 'open_file',
            payload: { path },
        }));
    } else {
        console.log('DEBUG: WebSocket not ready');
        showStatus('Not connected to server', 'error');
    }
};

// Configure LSP from UI
window.configureLSPFromUI = (clangdPath, compileCommandsDir) => {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({
            type: 'configure_lsp',
            payload: {
                clangdPath: clangdPath || 'clangd',
                compileCommandsDir,
            },
        }));
    } else {
        showStatus('Not connected to server', 'error');
    }
};

// Save file
window.saveFile = () => {
    if (!currentFilePath || !editor) {
        showStatus('No file open', 'error');
        return;
    }

    const content = editor.state.doc.toString();

    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({
            type: 'save',
            payload: {
                path: currentFilePath,
                content,
            },
        }));
    } else {
        showStatus('Not connected to server', 'error');
    }
};

// Handle LSP notifications
function handleLSPNotification(payload) {
    const notification = typeof payload === 'string' ? JSON.parse(payload) : payload;

    if (notification.method === 'textDocument/publishDiagnostics') {
        const diagnostics = notification.params.diagnostics || [];
        currentDiagnostics = diagnostics.map(diag => ({
            from: positionToOffset(editor.state.doc, diag.range.start),
            to: positionToOffset(editor.state.doc, diag.range.end),
            severity: diag.severity === 1 ? 'error' : diag.severity === 2 ? 'warning' : 'info',
            message: diag.message,
        }));
        window.currentDiagnostics = currentDiagnostics;  // Update window reference

        // Trigger re-lint
        if (editor) {
            editor.dispatch({});
        }
    }
}

// Handle LSP responses (for future completion support)
function handleLSPResponse(payload) {
    console.log('LSP response:', payload);

    const response = typeof payload === 'string' ? JSON.parse(payload) : payload;

    console.log('Response ID:', response.id);
    console.log('Pending requests:', Array.from(pendingCompletionRequests.keys()));

    // Handle completion responses
    if (response.id && pendingCompletionRequests.has(response.id)) {
        console.log('Found pending request for ID:', response.id);
        const resolve = pendingCompletionRequests.get(response.id);
        pendingCompletionRequests.delete(response.id);

        // LSP can return array or object with items
        let result = response.result;
        if (Array.isArray(result)) {
            result = { items: result };
        }

        resolve(result);
    }
}

// Map LSP completion kinds to CodeMirror types
function getLSPCompletionKind(kind) {
    const kindMap = {
        1: 'text',          // Text
        2: 'method',        // Method
        3: 'function',      // Function
        4: 'constructor',   // Constructor
        5: 'field',         // Field
        6: 'variable',      // Variable
        7: 'class',         // Class
        8: 'interface',     // Interface
        9: 'module',        // Module
        10: 'property',     // Property
        11: 'unit',         // Unit
        12: 'value',        // Value
        13: 'enum',         // Enum
        14: 'keyword',      // Keyword
        15: 'snippet',      // Snippet
        16: 'color',        // Color
        17: 'file',         // File
        18: 'reference',    // Reference
        19: 'folder',       // Folder
        20: 'enum-member',  // EnumMember
        21: 'constant',     // Constant
        22: 'struct',       // Struct
        23: 'event',        // Event
        24: 'operator',     // Operator
        25: 'type'          // TypeParameter
    };
    return kindMap[kind] || 'text';
}

// Convert LSP position to CodeMirror offset
function positionToOffset(doc, position) {
    const line = doc.line(position.line + 1);
    return line.from + position.character;
}

// Convert CodeMirror offset to LSP position
function offsetToPosition(doc, offset) {
    const line = doc.lineAt(offset);
    return {
        line: line.number - 1,
        character: offset - line.from,
    };
}

// Initialize on load
window.addEventListener('DOMContentLoaded', () => {
    createEditor('// Open a file to start editing', '');
    connectWebSocket();
});
