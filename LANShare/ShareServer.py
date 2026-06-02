from flask import Flask, request, send_from_directory, render_template_string, redirect, url_for, abort
import os
import socket
import logging
import datetime
from threading import Lock
from flask import send_file

app = Flask(__name__)
BASE_DIR = os.path.dirname(os.path.abspath(__file__))
app.config['UPLOAD_FOLDER'] = os.path.join(BASE_DIR, 'uploads')
app.config['MAX_CONTENT_LENGTH'] = 1 * 1024 * 1024 * 1024  # 限制上传大小为1GB

# 确保上传文件夹存在
os.makedirs(app.config['UPLOAD_FOLDER'], exist_ok=True)

# 配置日志
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

clipboard_content = ""
clipboard_lock = Lock()

# HTML
HTML = '''
<!DOCTYPE html>
<html>
<head>
    <title>资源服务器</title>
    <link rel="icon" href="/favicon.ico" type="image/x-icon">
    <style>
        body { font-family: Arial, sans-serif; max-width: 800px; margin: 0 auto; padding: 20px; }
        h1 { color: #333; }
        ul { list-style-type: none; padding: 0; }
        li { margin: 10px 0; padding: 10px; background-color: #f5f5f5; border-radius: 5px; }
        a { color: #0066cc; text-decoration: none; }
        a:hover { text-decoration: underline; }
        .file-row { display: flex; align-items: center; gap: 12px; justify-content: space-between; }
        .file-main { min-width: 0; }
        .file-name { word-break: break-all; }
        .file-meta { color: #666; font-size: 13px; margin-top: 4px; }
        .file-actions { display: flex; gap: 8px; flex-shrink: 0; }
        form { margin: 20px 0; padding: 15px; background-color: #f9f9f9; border-radius: 5px; }
        input[type=submit], button { background-color: #4CAF50; color: white; padding: 10px 15px; border: none; border-radius: 4px; cursor: pointer; }
        input[type=submit]:hover, button:hover { background-color: #45a049; }
        .delete-form { margin: 0; padding: 0; background: transparent; }
        .delete-button { background-color: #c62828; }
        .delete-button:hover { background-color: #a91f1f; }
        textarea { width: 100%; height: 120px; font-size: 16px; }
    </style>
</head>
<body>
    <h1>资源服务器</h1>
    <form action="/upload" method="post" enctype="multipart/form-data">
        <h2>上传文件</h2>
        <input type="file" name="file" required>
        <input type="submit" value="上传">
    </form>
    <h2>下载文件</h2>
    <ul>
        {% for file in files %}
            <li>
                <div class="file-row">
                    <div class="file-main">
                        <a class="file-name" href="{{ url_for('download_file', filename=file.name) }}">{{ file.name }}</a>
                        <div class="file-meta">{{ file.size }} · {{ file.modified }}</div>
                    </div>
                    <div class="file-actions">
                        <form class="delete-form" action="{{ url_for('delete_file', filename=file.name) }}" method="post" onsubmit="return confirm('确定删除这个文件？');">
                            <button class="delete-button" type="submit">删除</button>
                        </form>
                    </div>
                </div>
            </li>
        {% else %}
            <li>暂无文件</li>
        {% endfor %}
    </ul>
    <form method="post">
        <h2>共享剪切板</h2>
        <textarea name="clipboard_content" required>{{ clipboard_content }}</textarea><br>
        <input type="submit" value="保存剪切板">
        <button type="submit" name="save_clipboard" value="1">保存为文件</button>
    </form>
</body>
</html>
'''

@app.route('/', methods=['GET', 'POST'])
def index():
    global clipboard_content
    if request.method == 'POST':
        if 'clipboard_content' in request.form:
            new_content = request.form.get('clipboard_content', '')
            with clipboard_lock:
                clipboard_content = new_content
            logger.info("剪切板内容已更新")
            # 检查是否点击了“保存为文件”按钮
            if 'save_clipboard' in request.form:
                now = datetime.datetime.now().strftime("%Y.%m.%d-%H.%M.%S")
                filename = f"clipboard_save_{now}.txt"
                file_path = os.path.join(app.config['UPLOAD_FOLDER'], filename)
                with open(file_path, 'w', encoding='utf-8') as f:
                    f.write(clipboard_content)
                logger.info(f"剪切板内容已保存为文件: {filename}")
        return redirect(url_for('index'))
    files = get_file_list()
    with clipboard_lock:
        content = clipboard_content
    return render_template_string(HTML, files=files, clipboard_content=content)

@app.route('/upload', methods=['POST'])
def upload_file():
    if 'file' not in request.files:
        return redirect(url_for('index'))
    
    file = request.files['file']
    if file.filename == '':
        return redirect(url_for('index'))
    
    if file:
        filename = make_unique_filename(file.filename)
        file.save(os.path.join(app.config['UPLOAD_FOLDER'], filename))
        logger.info(f"文件已上传: {filename}")
        return redirect(url_for('index'))
    
    return redirect(url_for('index'))

@app.route('/download/<filename>')
def download_file(filename):
    if not is_safe_file(filename):
        abort(404)
    logger.info(f"下载文件: {filename}")
    return send_from_directory(app.config['UPLOAD_FOLDER'], filename)

@app.route('/delete/<filename>', methods=['POST'])
def delete_file(filename):
    if not is_safe_file(filename):
        abort(404)
    file_path = os.path.join(app.config['UPLOAD_FOLDER'], filename)
    if os.path.exists(file_path):
        os.remove(file_path)
        logger.info(f"文件已删除: {filename}")
    return redirect(url_for('index'))

def get_local_ip():
    try:
        # 创建一个临时套接字连接到外部地址，获取本机IP
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
            s.connect(("8.8.8.8", 80))
            return s.getsockname()[0]
    except OSError:
        return "127.0.0.1"

@app.route('/favicon.ico')
def favicon():
    return send_file(os.path.join(BASE_DIR, 'favicon.ico'), mimetype='image/x-icon')

def make_unique_filename(filename):
    filename = sanitize_filename(filename)
    if not filename:
        filename = f"upload_{datetime.datetime.now().strftime('%Y%m%d_%H%M%S')}"

    name, ext = os.path.splitext(filename)
    candidate = filename
    index = 1
    while os.path.exists(os.path.join(app.config['UPLOAD_FOLDER'], candidate)):
        candidate = f"{name}_{index}{ext}"
        index += 1
    return candidate

def sanitize_filename(filename):
    filename = os.path.basename(filename).replace('\\', '_').replace('\x00', '').strip()
    return filename

def is_safe_file(filename):
    if filename != sanitize_filename(filename):
        return False
    upload_dir = os.path.abspath(app.config['UPLOAD_FOLDER'])
    file_path = os.path.abspath(os.path.join(upload_dir, filename))
    return file_path.startswith(upload_dir + os.sep) and os.path.isfile(file_path)

def get_file_list():
    files = []
    for filename in os.listdir(app.config['UPLOAD_FOLDER']):
        file_path = os.path.join(app.config['UPLOAD_FOLDER'], filename)
        if not os.path.isfile(file_path):
            continue
        stat = os.stat(file_path)
        files.append({
            'name': filename,
            'size': format_file_size(stat.st_size),
            'modified': datetime.datetime.fromtimestamp(stat.st_mtime).strftime('%Y-%m-%d %H:%M:%S')
        })
    return sorted(files, key=lambda item: item['modified'], reverse=True)

def format_file_size(size):
    units = ['B', 'KB', 'MB', 'GB']
    value = float(size)
    for unit in units:
        if value < 1024 or unit == units[-1]:
            if unit == 'B':
                return f"{int(value)} {unit}"
            return f"{value:.1f} {unit}"
        value /= 1024

if __name__ == '__main__':
    host = get_local_ip()
    port = 8000
    
    print(f"文件服务器已启动!")
    print(f"请访问: http://{host}:{port}")
    
    app.run(host='0.0.0.0', port=port, debug=False)
