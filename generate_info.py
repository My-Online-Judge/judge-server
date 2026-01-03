import hashlib
import json
import os
import sys


def generate_info(problem_id):
    # Adjust path to point to the sibling judge-api directory
    # Assumes script is run from judge-server directory
    base_dir = os.path.abspath(
        os.path.join(
            os.path.dirname(__file__),
            "..",
            "judge-api",
            "src",
            "main",
            "resources",
            "test_cases",
        )
    )
    test_case_dir = os.path.join(base_dir, problem_id)

    if not os.path.exists(test_case_dir):
        print(f"Error: Directory {test_case_dir} does not exist.")
        return

    test_cases = {}

    # List all files and find matching pairs
    files = os.listdir(test_case_dir)
    in_files = [f for f in files if f.endswith(".in")]

    # Sort files to ensure order (1.in, 2.in, ...)
    # Try to sort numerically if possible
    try:
        in_files.sort(key=lambda x: int(os.path.splitext(x)[0]))
    except ValueError:
        in_files.sort()

    for in_file in in_files:
        name = os.path.splitext(in_file)[0]
        out_file = name + ".out"

        if out_file not in files:
            print(f"Warning: {out_file} not found for {in_file}, skipping.")
            continue

        in_path = os.path.join(test_case_dir, in_file)
        out_path = os.path.join(test_case_dir, out_file)

        with open(in_path, "rb") as f:
            input_data = f.read()
        with open(out_path, "rb") as f:
            output_data = f.read()

        # Calculate stripped md5 (remove whitespace from right)
        # Decode to string for rstrip(), then encode back
        try:
            output_str = output_data.decode("utf-8").rstrip()
            stripped_md5 = hashlib.md5(output_str.encode("utf-8")).hexdigest()
        except UnicodeDecodeError:
            # Fallback for binary data if needed, though usually text
            stripped_md5 = hashlib.md5(output_data).hexdigest()

        test_cases[name] = {
            "input_name": in_file,
            "input_size": len(input_data),
            "output_name": out_file,
            "output_size": len(output_data),
            "output_md5": hashlib.md5(output_data).hexdigest(),
            "stripped_output_md5": stripped_md5,
        }

    if not test_cases:
        print("No valid test cases found.")
        return

    info = {"test_case_number": len(test_cases), "spj": False, "test_cases": test_cases}

    info_path = os.path.join(test_case_dir, "info")
    with open(info_path, "w") as f:
        json.dump(info, f, indent=4)
    print(f"info file generated at {info_path}")


if __name__ == "__main__":
    if len(sys.argv) > 1:
        problem_id = sys.argv[1]
    else:
        problem_id = "simple-a-plus-b"
    generate_info(problem_id)
